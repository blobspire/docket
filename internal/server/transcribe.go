package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/store"
)

// Speech-to-text runs on a self-hosted whisper.cpp server ([transcribe] url):
// free, private, and given a glossary of the human's own project names, which
// generic dictation gets wrong.

const transcribeAttempts = 3

func (s *Server) transcribeTick(ctx context.Context) {
	ds, err := s.st.Dictations(store.DictTranscribing)
	if err != nil {
		return
	}
	for _, d := range ds {
		if ctx.Err() != nil {
			return
		}
		s.transcribeOne(ctx, d)
	}
}

func (s *Server) transcribeOne(ctx context.Context, d store.Dictation) {
	text, err := s.transcribe(ctx, d)
	if err == nil && text == "" {
		err = errors.New("no speech was found in the recording")
		d.Attempts = transcribeAttempts // trying again won't find any
	}
	out, editErr := s.st.EditDictation(d.ID, func(x *store.Dictation) error {
		if x.Status != store.DictTranscribing {
			return nil
		}
		if err != nil {
			x.Attempts = max(x.Attempts, d.Attempts) + 1
			x.Error = err.Error()
			if x.Attempts >= transcribeAttempts {
				x.Status = store.DictFailed
			}
			return nil
		}
		x.Transcript = text
		x.Text = joinText(x.Text, text)
		x.Attempts = 0
		if x.AutoSend {
			x.Status, x.SentAt = store.DictSent, time.Now().Unix()
		} else {
			x.Status = store.DictDraft
		}
		if !strings.HasPrefix(x.Error, "the recording stopped") {
			x.Error = ""
		}
		return nil
	})
	if editErr != nil {
		s.log.Printf("dictation %s: %v", d.ID, editErr)
		return
	}
	if err != nil {
		s.log.Printf("dictation %s: transcribe: %v", d.ID, err)
	}
	if out.Status == store.DictSent {
		s.kickIdeas()
	}
}

// joinText puts what the human typed before what they said.
func joinText(typed, said string) string {
	typed, said = strings.TrimSpace(typed), strings.TrimSpace(said)
	switch {
	case typed == "":
		return said
	case said == "":
		return typed
	}
	return typed + "\n\n" + said
}

// transcribe sends a dictation's WAV to the whisper server.
func (s *Server) transcribe(ctx context.Context, d store.Dictation) (string, error) {
	t := s.cfg.Transcribe
	if t.URL == "" {
		return "", errors.New("speech-to-text isn't set up: add [transcribe] url to the server config")
	}
	f, err := os.Open(s.blobPath(d.AudioSHA))
	if err != nil {
		return "", fmt.Errorf("the recording is missing: %w", err)
	}
	defer f.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "dictation.wav")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	mw.WriteField("response_format", "json")
	mw.WriteField("temperature", "0.0")
	mw.WriteField("language", t.Language)
	if g := s.glossary(); g != "" {
		mw.WriteField("prompt", g)
	}
	mw.Close()
	// A minute to spare, plus twice the recording's length.
	timeout := time.Minute + 2*time.Duration(d.AudioMS)*time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("the speech-to-text server didn't answer: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var res struct {
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("speech-to-text: HTTP %d: %s", resp.StatusCode, clipErr(string(raw)))
	}
	if res.Error != "" || resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("speech-to-text: HTTP %d: %s", resp.StatusCode, clipErr(firstNonEmptyStr(res.Error, string(raw))))
	}
	return cleanTranscript(res.Text), nil
}

// Whisper's markers for silence and sounds, which aren't words.
var (
	noiseTag   = regexp.MustCompile(`\[[A-Za-z _]+\]`)
	noiseParen = regexp.MustCompile(`(?i)\((?:[a-z ]*music[a-z ]*|silence|applause|laugh(?:s|ing|ter)?|inaudible|blank_audio|sighs?|coughs?|background noise)\)`)
)

func cleanTranscript(t string) string {
	t = noiseTag.ReplaceAllString(t, " ")
	t = noiseParen.ReplaceAllString(t, " ")
	return strings.Join(strings.Fields(t), " ")
}

// glossary primes speech-to-text with the words it must spell right: the
// person, their projects, the components and places in use, the leads, and
// the configured vocabulary. Whisper reads about 220 tokens of it.
func (s *Server) glossary() string {
	seen := map[string]bool{}
	var words []string
	add := func(w string) {
		w = strings.TrimSpace(w)
		if w == "" || seen[strings.ToLower(w)] || w == "the user" {
			return
		}
		seen[strings.ToLower(w)] = true
		words = append(words, w)
	}
	add(s.cfg.Person)
	add("docket")
	for _, name := range sortedKeys(s.cfg.Transcribe.SaidAs) {
		add(name + " (pronounced " + s.cfg.Transcribe.SaidAs[name] + ")")
		seen[strings.ToLower(name)] = true
	}
	for _, w := range s.cfg.Transcribe.Vocabulary {
		add(w)
	}
	if ps, err := s.st.Projects(false); err == nil {
		sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
		for _, p := range ps {
			add(p.Name)
		}
	}
	for _, l := range s.cfg.Leads {
		add(l.Name)
	}
	if items, err := s.st.Items("open", "snoozed", "answered"); err == nil {
		for _, it := range items {
			add(it.Component)
			add(it.Place)
		}
	}
	// A sentence in the style of the dictation: whisper copies the prompt's
	// style, and a bare list makes the whole transcript lowercase.
	person := s.cfg.Person
	if person == "" || person == "the user" {
		person = "me"
	}
	var picked []string
	n := 0
	for _, w := range words {
		if strings.EqualFold(w, s.cfg.Person) {
			continue
		}
		if n+len(w)+2 > 520 {
			break
		}
		picked = append(picked, w)
		n += len(w) + 2
	}
	if len(picked) == 0 {
		return ""
	}
	list := strings.Join(picked, ", ")
	if len(picked) > 1 {
		list = strings.Join(picked[:len(picked)-1], ", ") + ", or " + picked[len(picked)-1]
	}
	return "Okay. This is " + person + ", dictating an idea for one of my projects: " + list + ". Here it is."
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pruneAudio deletes recordings older than [transcribe] keep_audio; their
// transcripts stay.
func (s *Server) pruneAudio() {
	keep, err := parseEvery(s.cfg.Transcribe.KeepAudio)
	if err != nil || keep <= 0 {
		return
	}
	old, err := s.st.OldAudio(time.Now().Add(-keep).Unix())
	if err != nil {
		return
	}
	for _, d := range old {
		var refs int
		s.st.DB.QueryRow(`SELECT COUNT(*) FROM evidence WHERE sha256=?`, d.AudioSHA).Scan(&refs)
		if refs == 0 {
			os.Remove(s.blobPath(d.AudioSHA))
		}
		s.st.EditDictation(d.ID, func(x *store.Dictation) error { x.AudioSHA = ""; return nil })
	}
}
