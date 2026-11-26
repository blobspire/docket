package store

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// A project's timeline is its history as the human saw it: the results
// agents sent, what they asked of the human, the progress they posted, the
// problems they raised, what the human decided, and what shipped. It's built from what docket already
// keeps; nothing is copied for it.

// Timeline entry kinds, which are also its filters.
const (
	TLResult   = "result"
	TLAction   = "action"
	TLProgress = "progress"
	TLIssue    = "issue"
	TLDecision = "decision"
	TLShipped  = "shipped"
)

// TimelineFilters are the timeline's filters, in the order its chips show.
var TimelineFilters = []string{TLResult, TLAction, TLProgress, TLDecision, TLIssue, TLShipped}

// TimelineEntry is one moment on a project's timeline.
type TimelineEntry struct {
	At       int64
	Kind     string    // TLResult …
	Item     *Item     // a result, action item, progress entry or issue; for a decision, the item decided on
	Decision *Decision // what the human did (TLDecision)
	Idea     *Idea     // what shipped (TLShipped)
}

// Kinds of items on the timeline, and the decisions it shows.
const (
	tlResultKinds = `'review','fyi'`
	tlActionKinds = `'ask','approval','human_task'`
	// Items the old hooks guessed from a session, before agents filed them
	// (the upgrade dismissed the open ones), aren't part of the history.
	tlFiled      = `source NOT IN ('backfill','notification','permission_denied')`
	tlIssueKinds = `'issue','blocker'`
	tlDecisions  = `'approve','decline','choose','answer'`
)

// Timeline returns a project's entries before the unix time before (0:
// now), newest first; filter is one of TimelineFilters or "" for all, and an
// empty project means every project. It returns about limit entries: the
// oldest second it reaches is always whole (entries can share a second), so
// the next page, before that second, misses none. more reports older ones.
func (s *Store) Timeline(project, filter string, before int64, limit int) (entries []TimelineEntry, more bool, err error) {
	if before <= 0 {
		before = 1 << 62
	}
	// One more than a page from each source tells whether there's more.
	all, err := s.timeline(project, filter, "<", before, limit+1)
	if err != nil || len(all) <= limit {
		return all, false, err
	}
	// Each source gave its newest limit+1 entries, so every entry newer
	// than the limit-th is here; the limit-th's second may have more.
	edge := all[limit-1].At
	for _, e := range all {
		if e.At > edge {
			entries = append(entries, e)
		}
	}
	same, err := s.timeline(project, filter, "=", edge, 1<<20)
	if err != nil {
		return nil, false, err
	}
	older, err := s.timeline(project, filter, "<", edge, 1)
	return append(entries, same...), len(older) > 0, err
}

// timeline gathers the entries whose time compares (op: "<" or "=") with t,
// at most limit from each source, newest first.
func (s *Store) timeline(project, filter, op string, t int64, limit int) ([]TimelineEntry, error) {
	want := func(k string) bool { return filter == "" || filter == k }
	var out []TimelineEntry
	scope, args := "", []any{t}
	if project != "" {
		scope = ` AND project=?`
		args = append(args, project)
	}
	args = append(args, limit)
	items := func(kind, where string) error {
		rows, err := s.items(`SELECT `+itemCols+` FROM items WHERE `+where+` AND created_at`+op+`?`+scope+` ORDER BY created_at DESC, rowid DESC LIMIT ?`, args...)
		if err != nil {
			return err
		}
		for i := range rows {
			out = append(out, TimelineEntry{At: rows[i].CreatedAt, Kind: kind, Item: &rows[i]})
		}
		return nil
	}
	// Results the human dismissed or marked not worth it aren't kept for
	// later; everything else, done or still waiting, is.
	if want(TLResult) {
		if err := items(TLResult, `kind IN (`+tlResultKinds+`) AND status<>'dismissed' AND `+tlFiled); err != nil {
			return nil, err
		}
	}
	// Action items stay too, done or still open, so a past one is easy to
	// find again; one the human dismissed didn't matter.
	if want(TLAction) {
		if err := items(TLAction, `kind IN (`+tlActionKinds+`) AND status<>'dismissed' AND source<>'email' AND `+tlFiled); err != nil {
			return nil, err
		}
	}
	if want(TLProgress) {
		if err := items(TLProgress, `kind='`+KindProgress+`' AND status='`+StatusLogged+`'`); err != nil {
			return nil, err
		}
	}
	if want(TLIssue) {
		if err := items(TLIssue, `kind IN (`+tlIssueKinds+`) AND status<>'dismissed' AND source<>'email' AND `+tlFiled); err != nil {
			return nil, err
		}
	}
	if want(TLDecision) {
		q := `SELECT d.item_id, d.action, d.answer, d.context, i.title, d.ts, ` + prefixed("i.", itemCols) + ` FROM decisions d
			JOIN items i ON i.id = d.item_id
			WHERE d.action IN (` + tlDecisions + `) AND i.kind NOT IN ('rule','email') AND i.source<>'email' AND d.ts` + op + `?`
		dargs := []any{t}
		if project != "" {
			q += ` AND i.project=?`
			dargs = append(dargs, project)
		}
		q += ` ORDER BY d.ts DESC, d.id DESC LIMIT ?`
		dargs = append(dargs, limit)
		rows, err := s.DB.Query(q, dargs...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var d Decision
			dest := []any{&d.ItemID, &d.Action, &d.Answer, &d.Context, &d.Title, &d.TS}
			it, err := scanItem(scanAfter{rows, dest})
			if err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, TimelineEntry{At: d.TS, Kind: TLDecision, Item: &it, Decision: &d})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if want(TLShipped) {
		where := `WHERE status=? AND ended_at>0 AND ended_at` + op + `?`
		iargs := []any{IdeaBuilt, t}
		if project != "" {
			where += ` AND project=?`
			iargs = append(iargs, project)
		}
		ideas, err := s.ideaList(where+` ORDER BY ended_at DESC LIMIT ?`, append(iargs, limit)...)
		if err != nil {
			return nil, err
		}
		for i := range ideas {
			out = append(out, TimelineEntry{At: ideas[i].EndedAt, Kind: TLShipped, Idea: &ideas[i]})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].At > out[b].At })
	return out, nil
}

// scanAfter scans a row whose first columns go to dest and the rest to the
// scanner it's handed to (scanItem), so one query can carry both.
type scanAfter struct {
	rows interface{ Scan(...any) error }
	dest []any
}

func (s scanAfter) Scan(rest ...any) error { return s.rows.Scan(append(s.dest, rest...)...) }

// RecentProgress returns progress entries posted since the unix time since,
// newest first; an empty project means every project.
func (s *Store) RecentProgress(project string, since int64, limit int) ([]Item, error) {
	q, args := `SELECT `+itemCols+` FROM items WHERE kind=? AND status=? AND created_at>=?`, []any{KindProgress, StatusLogged, since}
	if project != "" {
		q += ` AND project=?`
		args = append(args, project)
	}
	return s.items(q+` ORDER BY created_at DESC, rowid DESC LIMIT ?`, append(args, limit)...)
}

// progressDigest changes when a progress entry is posted or changes, or when
// a question the human asked about an item in the last day moves along or
// is answered: the pages that show those refresh in place.
func (s *Store) progressDigest() string {
	h := sha256.New()
	var n, upd int64
	s.DB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(updated_at),0) FROM items WHERE status=?`, StatusLogged).Scan(&n, &upd)
	fmt.Fprintf(h, "%d.%d\n", n, upd)
	rows, err := s.DB.Query(`SELECT d.id, d.status, i.updated_at + d.answered_at FROM deliveries d JOIN items i ON i.id = d.item_id
		WHERE d.kind='ask' AND d.created_at>=? ORDER BY d.id`, now()-24*60*60)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, status string
			var iupd int64
			rows.Scan(&id, &status, &iupd)
			fmt.Fprintf(h, "%s\x00%s\x00%d\n", id, status, iupd)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:6])
}

// AnswerChoice is the option a decision's logged answer says the human
// picked ("Chose: Build it." → "Build it"), matched against the item's
// options (an option can hold a dot), or "".
func AnswerChoice(answer string, options []string) string {
	a := strings.TrimSpace(answer)
	best := ""
	for _, o := range options {
		if strings.HasPrefix(a, "Chose: "+o+".") && len(o) > len(best) {
			best = o
		}
	}
	return best
}
