package server

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/blobspire/docket/internal/store"
)

// The Ideas page: what the human dictated and what the agents are doing
// with it. Only drafts and failures need them here; finished proposals are
// on the docket like any other item.

type ideasView struct {
	Needs    []dictCard // drafts to send, recordings that failed
	Stuck    []ideaCard // ideas that failed
	Working  []ideaCard // being routed, researched or built
	Pending  []dictCard // dictations still being transcribed or routed
	Proposed []ideaCard // proposals waiting on the human
	Saved    []savedGroup
	Recent   []ideaCard // finished lately
	Empty    bool
}

type ideaCard struct {
	store.Idea
	Label      string // what's happening, in words
	Tone       string // ok | busy | wait | bad
	Dict       store.Dictation
	SessionURL string
	Session    string // the running session's name, when it has one
}

type dictCard struct {
	store.Dictation
	Label string // what's happening, in words
}

type savedGroup struct {
	Project string
	Ideas   []ideaCard
}

func (s *Server) pageIdeas(w http.ResponseWriter, r *http.Request) {
	v := &ideasView{}
	now := time.Now()
	dicts, _ := s.st.RecentDictations(now.Add(-30 * 24 * time.Hour).Unix())
	byID := map[string]store.Dictation{}
	for _, d := range dicts {
		byID[d.ID] = d
		switch d.Status {
		case store.DictDraft:
			v.Needs = append(v.Needs, dictCard{d, "Transcribed: check it, then send it"})
		case store.DictFailed:
			v.Needs = append(v.Needs, dictCard{d, firstNonEmptyStr(d.Error, "Something went wrong")})
		case store.DictRecording:
			v.Pending = append(v.Pending, dictCard{d, "Recording…"})
		case store.DictTranscribing:
			t := "Transcribing…"
			if d.Error != "" && d.Attempts > 0 {
				t = "Transcribing (retrying: " + d.Error + ")"
			}
			v.Pending = append(v.Pending, dictCard{d, t})
		case store.DictSent, store.DictRouting:
			t := "Working out where it goes…"
			if d.Error != "" && d.Attempts > 0 {
				t = "Routing (retrying: " + d.Error + ")"
			}
			v.Pending = append(v.Pending, dictCard{d, t})
		}
	}
	ideas, _ := s.st.RecentIdeas(now.Add(-14 * 24 * time.Hour).Unix())
	running := 0
	for _, i := range ideas {
		if store.IdeaRunning(i.Status) {
			running++
		}
	}
	saved := map[string][]ideaCard{}
	for _, i := range ideas {
		c := ideaCard{Idea: i, Dict: byID[i.DictationID]}
		if c.Dict.ID == "" && i.DictationID != "" {
			c.Dict, _, _ = s.st.GetDictation(i.DictationID)
		}
		if i.SessionID != "" {
			if sess, ok, _ := s.st.GetSession(i.SessionID); ok {
				if sess.State != "ended" {
					c.SessionURL = rcURL(sess.BridgeID)
				}
				c.Session = firstNonEmptyStr(sess.Name, i.SessionName)
			}
		}
		c.Label, c.Tone = s.ideaStatusText(i, running, c.Session)
		switch i.Status {
		case store.IdeaFailed:
			v.Stuck = append(v.Stuck, c)
		case store.IdeaProposed:
			v.Proposed = append(v.Proposed, c)
		case store.IdeaSaved:
			saved[i.Project] = append(saved[i.Project], c)
		case store.IdeaBuilt, store.IdeaSent, store.IdeaDismissed:
			v.Recent = append(v.Recent, c)
		default:
			v.Working = append(v.Working, c)
		}
	}
	for p, cs := range saved {
		v.Saved = append(v.Saved, savedGroup{Project: p, Ideas: cs})
	}
	sort.Slice(v.Saved, func(i, j int) bool { return projectLess(v.Saved[i].Project, v.Saved[j].Project) })
	v.Empty = len(v.Needs)+len(v.Stuck)+len(v.Working)+len(v.Pending)+len(v.Proposed)+len(v.Saved)+len(v.Recent) == 0
	s.render(w, "ideas.html", page{Title: "Ideas", Nav: "ideas", Ideas: v, Error: r.URL.Query().Get("err")})
}

func (s *Server) ideaStatusText(i store.Idea, running int, session string) (string, string) {
	where := firstNonEmptyStr(i.Place, i.Machine)
	switch i.Status {
	case store.IdeaQueued:
		if running >= s.cfg.Ideas.MaxRunning {
			return "Waiting for a free agent (" + strconv.Itoa(running) + " already working)", "wait"
		}
		return "About to start", "busy"
	case store.IdeaWaiting:
		return "Waiting for the machine with its code to wake", "wait"
	case store.IdeaStarting:
		return "Starting a session on " + where, "busy"
	case store.IdeaResearching:
		return "Researching on " + where, "busy"
	case store.IdeaBuilding:
		return "Building on " + where, "busy"
	case store.IdeaProposed:
		return "Proposal ready for you", "ok"
	case store.IdeaBuilt:
		return "Shipped", "ok"
	case store.IdeaSaved:
		if i.Note != "" {
			return "Saved, with an agent's notes", "wait"
		}
		return "Saved for later", "wait"
	case store.IdeaSent:
		return "Sent to " + firstNonEmptyStr(session, "a running session"), "ok"
	case store.IdeaDismissed:
		return "Dismissed", "wait"
	case store.IdeaFailed:
		return firstNonEmptyStr(i.Error, "Stuck"), "bad"
	}
	return i.Status, "wait"
}

// ideaAttention counts what needs the human on the Ideas page: drafts to
// send and anything that failed.
func (s *Server) ideaAttention() int {
	var n int
	s.st.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM dictations WHERE status IN (?,?) AND created_at>=?) + (SELECT COUNT(*) FROM ideas WHERE status=?)`,
		store.DictDraft, store.DictFailed, time.Now().Add(-30*24*time.Hour).Unix(), store.IdeaFailed).Scan(&n)
	return n
}
