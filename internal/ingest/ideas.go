package ingest

import (
	"encoding/json"
	"strings"

	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/store"
)

// ideaRecord applies `docket idea done|save|take`: an agent reporting on an
// idea the human dictated.
func ideaRecord(s *store.Store, r record.Record, sid string) error {
	var p struct {
		Op  string `json:"op"`
		ID  string `json:"id"`
		Why string `json:"why"`
	}
	_ = json.Unmarshal(r.Payload, &p)
	if p.ID == "" {
		return nil
	}
	if _, ok, err := s.GetIdea(p.ID); err != nil || !ok {
		return err
	}
	why := clip(strings.TrimSpace(p.Why), 4000)
	var item string
	_, err := s.EditIdea(p.ID, func(i *store.Idea) error {
		switch p.Op {
		case "done":
			i.Status = store.IdeaBuilt
		case "save":
			i.Status = store.IdeaSaved
		case "take":
			if store.IdeaRunning(i.Status) || i.Status == store.IdeaProposed {
				return nil // someone is already on it
			}
			i.Status = store.IdeaResearching
			i.SessionID, i.Machine, i.Error = sid, r.Machine, ""
			if i.StartedAt == 0 {
				i.StartedAt = parseTS(r.TS)
			}
			return nil
		default:
			return nil
		}
		if why != "" {
			i.Note = why
		}
		i.Error = ""
		item = i.ItemID
		return nil
	})
	if err != nil {
		return err
	}
	// The proposal is settled: it no longer waits on anyone.
	if item != "" {
		note := "the idea shipped"
		if p.Op == "save" {
			note = "the idea was saved for later"
		}
		return closeItem(s, item, firstNonEmpty(why, note))
	}
	return nil
}

// linkProposal ties a `docket propose` item to its idea: the idea now waits
// on the human. A newer proposal replaces the old one.
func linkProposal(s *store.Store, ideaID, itemID, sid, machine string) error {
	if _, ok, err := s.GetIdea(ideaID); err != nil || !ok {
		return err
	}
	old := ""
	_, err := s.EditIdea(ideaID, func(i *store.Idea) error {
		old = i.ItemID
		i.ItemID = itemID
		if i.Status != store.IdeaBuilt {
			i.Status = store.IdeaProposed
		}
		if i.SessionID == "" && sid != "" {
			i.SessionID = sid
			if machine != "" {
				i.Machine = machine
			}
		}
		i.Error = ""
		return nil
	})
	if err != nil {
		return err
	}
	if old != "" && old != itemID {
		return closeItem(s, old, "replaced by a newer proposal")
	}
	return nil
}

// sessionStarted applies a push machine's report on a session docket asked
// it to start.
func sessionStarted(s *store.Store, r record.Record) error {
	var p struct {
		StartID string `json:"start_id"`
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(r.Payload, &p)
	if p.StartID == "" {
		return nil
	}
	st, changed, err := s.StartResult(p.StartID, p.OK, clip(p.Error, 500))
	if err != nil || !changed || p.OK || st.IdeaID == "" || st.Op != "start" {
		return err
	}
	// It didn't start there: back in the queue, where placement skips this
	// machine for it.
	_, err = s.EditIdea(st.IdeaID, func(i *store.Idea) error {
		if i.Status == store.IdeaStarting && i.SessionID == "" {
			i.Status = store.IdeaQueued
			i.Error = "couldn't start on " + st.Machine + ": " + clip(p.Error, 300)
		}
		return nil
	})
	return err
}

func closeItem(s *store.Store, id, note string) error {
	it, ok, err := s.GetItem(id)
	if err != nil || !ok {
		return err
	}
	if !active(it.Status) && it.Status != "delivered" {
		return nil
	}
	return s.SetStatus(id, "done", note, "")
}
