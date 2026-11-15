package server

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/agentctx"
	"github.com/blobspire/docket/internal/store"
)

// ContextFor builds ~/.docket/context.json for one machine: every project
// (with the components and places in use), that machine's folder mappings,
// each lead's project, and that machine's own open items. Other machines'
// items stay off it (a shared lab computer shouldn't list personal admin).
// The content is deterministic, so its hash only changes when something did.
func (s *Server) ContextFor(machine string) ([]byte, error) {
	projects, err := s.st.Projects(false)
	if err != nil {
		return nil, err
	}
	items, err := s.st.Items("open", "snoozed", "answered")
	if err != nil {
		return nil, err
	}
	type agg struct {
		open               int
		components, places map[string]bool
	}
	per := map[string]*agg{}
	get := func(p string) *agg {
		if per[p] == nil {
			per[p] = &agg{components: map[string]bool{}, places: map[string]bool{}}
		}
		return per[p]
	}
	places := map[string]bool{}
	c := agentctx.Context{Person: s.cfg.Person, Machine: machine, Leads: map[string]string{}}
	for _, it := range items {
		if it.Kind == "rule" {
			continue
		}
		a := get(it.Project)
		a.open++
		if it.Component != "" {
			a.components[it.Component] = true
		}
		if it.Place != "" {
			a.places[it.Place] = true
			places[it.Place] = true
		}
		if it.Machine == machine {
			c.Open = append(c.Open, agentctx.Item{ID: it.ID, Title: it.Title, Kind: it.Kind, Project: it.Project, Urgency: it.Urgency,
				Key: store.AgentKey(it.DedupeKey), SessionID: it.SessionID, CreatedAt: it.CreatedAt})
		}
	}
	var asOf int64
	for _, p := range projects {
		a := get(p.Name)
		c.Projects = append(c.Projects, agentctx.Project{Name: p.Name, Description: p.Description, Components: keys(a.components), Places: keys(a.places), Open: a.open})
		asOf = max(asOf, p.UpdatedAt)
	}
	for _, it := range items {
		if it.Machine == machine {
			asOf = max(asOf, it.UpdatedAt)
		}
	}
	c.Places = keys(places)
	dirs, _ := s.st.ProjectDirs(machine)
	for _, d := range dirs {
		c.Dirs = append(c.Dirs, agentctx.Dir{Dir: d.Dir, Project: d.Project})
	}
	for _, l := range s.cfg.Leads {
		c.Leads[l.Name] = l.Project
	}
	sort.Slice(c.Open, func(i, j int) bool { return c.Open[i].ID < c.Open[j].ID })
	// What the server has processed from this machine lately, open or not:
	// the machine's own record of its filings defers to the server for these.
	// Day-aligned, so the window doesn't change the file every second.
	day := s.cfg.Now().Truncate(24 * time.Hour).Add(-48 * time.Hour).Unix()
	c.Seen = s.st.SeenIDs(machine, day)
	c.Timezone = s.cfg.Timezone // "" when unset: the machine's own zone is as good a guess
	// "As of" is the last change the file reflects, not the time it was made,
	// so an unchanged queue gives byte-identical files.
	var lastDir int64
	s.st.DB.QueryRow(`SELECT COALESCE(MAX(updated_at),0) FROM project_dirs WHERE machine=?`, machine).Scan(&lastDir)
	c.AsOf = max(asOf, lastDir)
	return json.Marshal(c)
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// syncContext sends a local or pull machine its context when it changed.
func (s *Server) syncContext(ctx context.Context, m Machine) {
	data, err := s.ContextFor(m.Name)
	if err != nil {
		s.log.Printf("%s: context: %v", m.Name, err)
		return
	}
	h := agentctx.Hash(data)
	if s.st.Get("context:sent:"+m.Name) == h {
		return
	}
	if _, err := s.remote(ctx, m, strings.NewReader(string(data)), "context", "put"); err != nil {
		s.log.Printf("%s: context put: %v", m.Name, err)
		// An older docket there doesn't take a context: don't retry this
		// exact content every pull; a change (or an upgrade) tries again.
		if !strings.Contains(err.Error(), `unknown command "context"`) {
			return
		}
	}
	s.st.Set("context:sent:"+m.Name, h)
}

// leadProject maps a lead to its project, for ingest.
func (s *Server) leadProject(lead string) string {
	if l, ok := s.cfg.Lead(lead); ok {
		return l.Project
	}
	return ""
}
