package server

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/store"
	"github.com/blobspire/docket/internal/ulid"
)

// Dictation capture: the page records 16 kHz mono 16-bit PCM and uploads it
// in numbered chunks as the human talks, so a dropped signal or a closed tab
// loses seconds at most. Finishing wraps the chunks in a WAV file, stores it
// like any other blob, and queues it for speech-to-text.

const (
	pcmRate        = 16000
	pcmBytesPerMS  = pcmRate * 2 / 1000 // 32
	maxChunk       = 1 << 20            // one upload (the page sends ~3 s, ~96 KB)
	maxAudioMS     = 60 * 60 * 1000     // an hour per dictation
	maxDictText    = 100 << 10
	staleRecording = 30 * time.Minute // no chunk for this long: the page is gone, finish what arrived
)

var dictIDOK = regexp.MustCompile(`^dict-[a-z0-9]{8}$`)

// errGone: the dictation was already finished (or dropped) and can't take
// more; the page carries what's left into a new dictation instead of losing it.
var errGone = errors.New("this recording was already finished")

func (s *Server) partPath(id string, seq int64) string {
	return filepath.Join(s.cfg.DataDir, "dictations", fmt.Sprintf("%s.%06d.pcm", id, seq))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fromFetch(r *http.Request) bool { return r.Header.Get("X-Requested-With") == "fetch" }

// dictationNew starts a dictation: a voice recording about to stream in, or
// typed text that goes straight to the router.
func (s *Server) dictationNew(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDictText+4096)
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too long"})
		return
	}
	text := strings.TrimSpace(r.PostFormValue("text"))
	voice := r.PostFormValue("source") == "voice"
	// The page names the dictation itself, so it can store chunks before it
	// reaches the server; a retry of the same id is the same dictation.
	id := r.PostFormValue("id")
	if id == "" {
		id = ulid.ShortID("dict")
	}
	if !dictIDOK.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad dictation id"})
		return
	}
	if old, ok, _ := s.st.GetDictation(id); ok {
		writeJSON(w, http.StatusOK, map[string]any{"id": old.ID, "status": old.Status})
		return
	}
	d := store.Dictation{ID: id, Login: r.Header.Get("Tailscale-User-Login"), Source: "text",
		Text: text, ProjectHint: store.Slug(r.PostFormValue("project")), AutoSend: r.PostFormValue("auto_send") != "0"}
	switch {
	case voice:
		if s.cfg.Transcribe.URL == "" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "speech-to-text isn't set up on the server: type it instead"})
			return
		}
		d.Source, d.Status = "voice", store.DictRecording
	case text == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "say or type something first"})
		return
	default:
		d.Status, d.SentAt = store.DictSent, time.Now().Unix()
	}
	if err := s.st.AddDictation(d); err != nil {
		if old, ok, _ := s.st.GetDictation(id); ok { // two tries at once
			writeJSON(w, http.StatusOK, map[string]any{"id": old.ID, "status": old.Status})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if d.Status == store.DictSent {
		s.kickIdeas()
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": d.ID, "status": d.Status})
}

// dictationAudio stores one chunk. Chunks arrive in order: a repeat is
// acknowledged and ignored, a gap is refused with the number expected.
func (s *Server) dictationAudio(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	seq, err := strconv.ParseInt(r.URL.Query().Get("seq"), 10, 64)
	if !dictIDOK.MatchString(id) || err != nil || seq < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad chunk"})
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChunk))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "chunk too large"})
		return
	}
	if len(data)%2 != 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "audio must be 16-bit samples"})
		return
	}
	// One at a time with finishing, so a chunk never lands after its
	// recording was assembled.
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	d, ok, err := s.st.GetDictation(id)
	if err != nil || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such dictation"})
		return
	}
	if seq < d.NextSeq {
		// Already have it (a retry): nothing to add.
		writeJSON(w, http.StatusOK, map[string]any{"next": d.NextSeq, "status": d.Status})
		return
	}
	if d.Status != store.DictRecording {
		writeJSON(w, http.StatusGone, map[string]any{"error": errGone.Error(), "next": d.NextSeq, "status": d.Status})
		return
	}
	if seq > d.NextSeq {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "missing an earlier chunk", "next": d.NextSeq})
		return
	}
	if d.AudioMS+int64(len(data)/pcmBytesPerMS) > maxAudioMS {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "an hour is the most one dictation takes", "next": d.NextSeq})
		return
	}
	// The part file first: if recording the new count fails, the retry
	// rewrites the same part.
	if err := writeFileAtomic(s.partPath(id, seq), data); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	d, err = s.st.EditDictation(id, func(d *store.Dictation) error {
		if d.Status != store.DictRecording || d.NextSeq != seq {
			return nil // a concurrent retry got here first
		}
		d.NextSeq++
		d.AudioMS += int64(len(data) / pcmBytesPerMS)
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"next": d.NextSeq, "status": d.Status})
}

// dictationFinish ends a recording and queues it for speech-to-text.
func (s *Server) dictationFinish(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, maxDictText+4096)
	if !dictIDOK.MatchString(id) || r.ParseForm() != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	var auto *bool
	if v := r.PostFormValue("auto_send"); v != "" {
		b := v != "0"
		auto = &b
	}
	d, err := s.finishDictation(id, auto, r.PostFormValue("project"), r.PostFormValue("text"), false)
	if errors.Is(err, errGone) {
		writeJSON(w, http.StatusGone, map[string]string{"error": err.Error(), "status": d.Status})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": d.ID, "status": d.Status})
}

// finishDictation wraps the chunks in a WAV blob and queues it. Finishing
// twice is harmless. cut marks a recording the server finished because the
// page stopped sending.
func (s *Server) finishDictation(id string, auto *bool, project, text string, cut bool) (store.Dictation, error) {
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	d, ok, err := s.st.GetDictation(id)
	if err != nil || !ok {
		return d, errors.New("no such dictation")
	}
	if d.Status != store.DictRecording {
		// Finishing twice is harmless; typed words it doesn't have yet are
		// refused (the page sends them as a new dictation).
		if t := strings.TrimSpace(text); t != "" && !strings.Contains(d.Text, t) {
			return d, errGone
		}
		return d, nil
	}
	sha, err := s.assembleWAV(id, d.NextSeq)
	if err != nil {
		return d, err
	}
	d, err = s.st.EditDictation(id, func(d *store.Dictation) error {
		if d.Status != store.DictRecording {
			return nil
		}
		if auto != nil {
			d.AutoSend = *auto
		}
		if p := store.Slug(project); p != "" {
			d.ProjectHint = p
		}
		if t := strings.TrimSpace(text); t != "" {
			d.Text = clipStr(t, maxDictText)
		}
		if cut {
			d.Error = "the recording stopped arriving; this is what reached docket"
		}
		switch {
		case sha != "":
			d.AudioSHA, d.Status = sha, store.DictTranscribing
		case strings.TrimSpace(d.Text) != "":
			d.Source, d.Status, d.SentAt = "text", store.DictSent, time.Now().Unix()
		default:
			d.Status, d.Error = store.DictDeleted, "nothing was recorded"
		}
		return nil
	})
	if err != nil {
		return d, err
	}
	for seq := int64(0); seq < d.NextSeq; seq++ {
		os.Remove(s.partPath(id, seq))
	}
	switch d.Status {
	case store.DictTranscribing:
		s.kickTranscribe()
	case store.DictSent:
		s.kickIdeas()
	}
	return d, nil
}

// assembleWAV joins a recording's chunks into one WAV blob and returns its
// hash ("" when nothing was recorded).
func (s *Server) assembleWAV(id string, parts int64) (string, error) {
	var size int64
	for seq := int64(0); seq < parts; seq++ {
		info, err := os.Stat(s.partPath(id, seq))
		if err != nil {
			return "", fmt.Errorf("chunk %d is missing: %w", seq, err)
		}
		size += info.Size()
	}
	if size == 0 {
		return "", nil
	}
	dir := filepath.Join(s.cfg.DataDir, "blobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".in-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	out := io.MultiWriter(tmp, h)
	if _, err := out.Write(wavHeader(size)); err != nil {
		tmp.Close()
		return "", err
	}
	for seq := int64(0); seq < parts; seq++ {
		f, err := os.Open(s.partPath(id, seq))
		if err != nil {
			tmp.Close()
			return "", err
		}
		_, err = io.Copy(out, f)
		f.Close()
		if err != nil {
			tmp.Close()
			return "", err
		}
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	sha := hex.EncodeToString(h.Sum(nil))
	if err := os.Rename(tmp.Name(), s.blobPath(sha)); err != nil {
		return "", err
	}
	return sha, nil
}

// wavHeader is the 44-byte header of a 16 kHz mono 16-bit PCM WAV file.
func wavHeader(dataSize int64) []byte {
	b := make([]byte, 44)
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+dataSize))
	copy(b[8:], "WAVE")
	copy(b[12:], "fmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1) // PCM
	binary.LittleEndian.PutUint16(b[22:], 1) // mono
	binary.LittleEndian.PutUint32(b[24:], pcmRate)
	binary.LittleEndian.PutUint32(b[28:], pcmRate*2)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(dataSize))
	return b
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".part-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// dictationGet reports a dictation's progress (the capture sheet polls it
// while it transcribes a recording the human wants to review).
func (s *Server) dictationGet(w http.ResponseWriter, r *http.Request) {
	d, ok, err := s.st.GetDictation(r.PathValue("id"))
	if err != nil || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such dictation"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": d.ID, "status": d.Status, "text": d.Text, "transcript": d.Transcript, "error": d.Error, "audio_ms": d.AudioMS})
}

// dictationAct handles the Ideas page's buttons on a dictation: send a
// reviewed draft, retry a failure, delete it.
func (s *Server) dictationAct(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, maxDictText+4096)
	if err := r.ParseForm(); err != nil {
		s.actDone(w, r, "/ideas", errors.New("too long"))
		return
	}
	action := r.PathValue("action")
	_, err := s.st.EditDictation(id, func(d *store.Dictation) error {
		switch action {
		case "send":
			if d.Status != store.DictDraft && !(d.Status == store.DictFailed && d.Transcript+d.Text != "") {
				return errors.New("only a draft can be sent")
			}
			if t := strings.TrimSpace(r.PostFormValue("text")); t != "" {
				d.Text = clipStr(t, maxDictText)
			}
			if strings.TrimSpace(d.Text) == "" {
				return errors.New("there's nothing to send")
			}
			if p := r.PostFormValue("project"); p != "" {
				d.ProjectHint = store.Slug(p)
			}
			d.Status, d.Error, d.SentAt, d.Attempts = store.DictSent, "", time.Now().Unix(), 0
		case "retry":
			if d.Status != store.DictFailed {
				return errors.New("only a failed dictation can be retried")
			}
			d.Attempts, d.Error = 0, ""
			if d.Transcript == "" && d.AudioSHA != "" {
				d.Status = store.DictTranscribing
			} else {
				d.Status, d.SentAt = store.DictSent, time.Now().Unix()
			}
		case "delete":
			if d.Status == store.DictRouting {
				return errors.New("it's being routed right now")
			}
			d.Status = store.DictDeleted
		default:
			return errors.New("unknown action")
		}
		return nil
	})
	if err == nil {
		s.kickTranscribe()
		s.kickIdeas()
	}
	s.actDone(w, r, "/ideas", err)
}

// actDone answers a button on the Ideas page: JSON for the page's fetch, a
// redirect back otherwise.
func (s *Server) actDone(w http.ResponseWriter, r *http.Request, back string, err error) {
	if fromFetch(r) {
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
		return
	}
	if err != nil {
		http.Redirect(w, r, back+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// finishStale finishes recordings whose page stopped sending (closed, out of
// signal for good): what arrived is transcribed rather than lost.
func (s *Server) finishStale() {
	ds, err := s.st.Dictations(store.DictRecording)
	if err != nil {
		return
	}
	for _, d := range ds {
		if time.Since(time.Unix(d.UpdatedAt, 0)) < staleRecording {
			continue
		}
		if _, err := s.finishDictation(d.ID, nil, "", "", true); err != nil {
			s.log.Printf("dictation %s: finishing a stale recording: %v", d.ID, err)
		}
	}
}
