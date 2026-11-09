package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/store"
	"github.com/blobspire/docket/internal/ulid"
)

// The chief of staff's two small model jobs, both optional and capped:
//   - pre-review: annotate items agents file (and send incomplete ones back)
//   - rule proposals: weekly, distil corrections and decisions into
//     preference rules the human approves; approved rules reach every agent
//
// Neither ever answers an item for the human.

// Chief is the [chief] section of the config.
type Chief struct {
	PreReview   *bool   `toml:"pre_review"`
	ReviewModel string  `toml:"review_model"`
	RulesModel  string  `toml:"rules_model"`
	DailyCap    int     `toml:"daily_cap"`   // model calls per day
	BudgetUSD   float64 `toml:"budget_usd"`  // per call
	Claude      string  `toml:"claude"`      // path to the claude CLI on the server
	RulesEvery  string  `toml:"rules_every"` // "7d"
}

func (c Chief) preReviewOn() bool { return c.PreReview == nil || *c.PreReview }

func (s *Server) chiefDefaults() Chief {
	c := s.cfg.Chief
	if c.ReviewModel == "" {
		c.ReviewModel = "sonnet"
	}
	if c.RulesModel == "" {
		c.RulesModel = "sonnet"
	}
	if c.DailyCap == 0 {
		c.DailyCap = 25
	}
	if c.BudgetUSD == 0 {
		c.BudgetUSD = 0.25
	}
	if c.Claude == "" {
		c.Claude = expand("~/.local/bin/claude")
		if _, err := os.Stat(c.Claude); err != nil {
			c.Claude = "claude"
		}
	}
	if c.RulesEvery == "" {
		c.RulesEvery = "7d"
	}
	return c
}

// ask runs one headless, tool-less model call that must return JSON matching
// schema. It never runs docket's hooks and keeps no session.
func (s *Server) ask(ctx context.Context, model, prompt, schema string) (json.RawMessage, error) {
	c := s.chiefDefaults()
	day := time.Now().Format("2006-01-02")
	callsKey := "chief:calls:" + day
	n, _ := strconv.Atoi(s.st.Get(callsKey))
	if n >= c.DailyCap {
		return nil, errors.New("daily model-call cap reached")
	}
	s.st.Set(callsKey, strconv.Itoa(n+1))
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Claude, "-p", "--model", model, "--safe-mode", "--no-session-persistence", "--tools", "",
		"--output-format", "json", "--max-budget-usd", strconv.FormatFloat(c.BudgetUSD, 'f', 2, 64), "--json-schema", schema, prompt)
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "DOCKET_INTERNAL=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("claude: %v %s", err, clipErr(errb.String()))
	}
	var res struct {
		IsError    bool            `json:"is_error"`
		Result     string          `json:"result"`
		Structured json.RawMessage `json:"structured_output"`
		Cost       float64         `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		return nil, fmt.Errorf("claude output: %w", err)
	}
	costKey := "chief:cost:" + day
	prev, _ := strconv.ParseFloat(s.st.Get(costKey), 64)
	s.st.Set(costKey, strconv.FormatFloat(prev+res.Cost, 'f', 4, 64))
	if res.IsError {
		return nil, errors.New("claude: " + clipErr(res.Result))
	}
	if len(res.Structured) > 0 && string(res.Structured) != "null" {
		return res.Structured, nil
	}
	return json.RawMessage(res.Result), nil
}

// ---- pre-review -----------------------------------------------------------

const reviewSchema = `{"type":"object","properties":{"note":{"type":"string"},"confidence":{"type":"number"},"missing":{"type":"string"}},"required":["note","confidence","missing"]}`

func (s *Server) preReviewTick(ctx context.Context) {
	if !s.chiefDefaults().preReviewOn() {
		return
	}
	items, err := s.st.Items("open")
	if err != nil {
		return
	}
	done := 0
	for _, it := range items {
		if done >= 2 {
			return // a couple per tick keeps it gentle
		}
		if it.Source != ingest.SrcCLI || it.ChiefNote != "" || (it.Kind != "ask" && it.Kind != "review") ||
			time.Since(time.Unix(it.CreatedAt, 0)) > 48*time.Hour || s.st.Get("pre:"+it.ID) != "" {
			continue
		}
		s.st.Set("pre:"+it.ID, "1")
		done++
		ev, _ := s.st.Evidence(it.ID)
		if it.Kind == "review" && len(ev) == 0 {
			s.bounce(it, "evidence for the review: screenshots, a short video, a report or a diff, with `docket add "+it.ID+" --evidence <file>`")
		}
		if err := s.preReview(ctx, it, len(ev)); err != nil {
			s.log.Printf("pre-review %s: %v", it.ID, err)
		}
	}
}

func (s *Server) preReview(ctx context.Context, it store.Item, evidence int) error {
	rules := s.rulesText()
	prompt := fmt.Sprintf(`You are the chief of staff for %[1]s, who runs many AI coding agents. An agent filed this item in %[1]s's queue. Annotate it so %[1]s can decide quickly. You never decide for them.

Kind: %[2]s
Title: %[3]s
Details: %[4]s
Options: %[5]s
Agent's recommendation: %[6]s
Evidence files attached: %[7]d

%[1]s's approved preferences:
%[8]s

Return JSON:
- note: one or two plain sentences. What is being decided, and anything %[1]s should weigh, including any approved preference that applies. Do not recommend beyond what the evidence supports.
- confidence: 0 to 1, how well this item alone lets %[1]s decide without opening the agent's session.
- missing: what the agent should add first, or "" if nothing important is missing.`,
		s.cfg.Person, it.Kind, it.Title, clipStr(it.Body, 3000), strings.Join(it.Options, " | "), it.Recommend, evidence, firstNonEmptyStr(rules, "(none yet)"))
	raw, err := s.ask(ctx, s.chiefDefaults().ReviewModel, prompt, reviewSchema)
	if err != nil {
		return err
	}
	var r struct {
		Note       string  `json:"note"`
		Confidence float64 `json:"confidence"`
		Missing    string  `json:"missing"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	note := strings.TrimSpace(r.Note)
	if r.Confidence < 0.4 && strings.TrimSpace(r.Missing) != "" {
		note += " Missing: " + strings.TrimSpace(r.Missing)
		s.bounce(it, strings.TrimSpace(r.Missing)+" (add it with `docket add "+it.ID+" --why \"...\"`)")
	}
	s.st.SetChiefNote(it.ID, clipStr(note, 1200))
	s.st.Bump()
	return nil
}

// bounce asks the filing agent, once per item, for what is missing.
func (s *Server) bounce(it store.Item, what string) {
	if it.SessionID == "" || s.st.Get("bounced:"+it.ID) != "" {
		return
	}
	s.st.Set("bounced:"+it.ID, "1")
	nonce := randomNonce()
	s.st.AddDelivery(store.Delivery{ID: ulid.New(time.Now()), ItemID: it.ID, Machine: it.Machine, SessionID: it.SessionID, Nonce: nonce,
		Text: fmt.Sprintf("[docket %s #%s] docket's chief of staff, before %s looks at %q: please add %s. This isn't %s's answer; keep working meanwhile.", it.ID, nonce, s.cfg.Person, it.Title, what, s.cfg.Person)})
	s.kickDeliver()
}

func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---- rules ----------------------------------------------------------------

const rulesSchema = `{"type":"object","properties":{"rules":{"type":"array","items":{"type":"object","properties":{"text":{"type":"string"},"scope":{"type":"string"},"evidence":{"type":"array","items":{"type":"string"}}},"required":["text","scope","evidence"]}}},"required":["rules"]}`

// ProposeRules distils recent corrections and decisions into proposed
// preference rules, filed as queue items for the human to approve.
func (s *Server) ProposeRules(ctx context.Context) (int, error) {
	since := time.Now().AddDate(0, 0, -14).Unix()
	ds, err := s.st.DecisionsSince(since, 200)
	if err != nil {
		return 0, err
	}
	if len(ds) < 3 {
		return 0, nil // not enough to learn from; spend nothing
	}
	var ev strings.Builder
	for _, d := range ds {
		switch d.Action {
		case "correction":
			fmt.Fprintf(&ev, "- [correction in %s] %s\n", firstNonEmptyStr(d.Context, "?"), clipStr(d.Answer, 300))
		default:
			fmt.Fprintf(&ev, "- [%s on %q] %s\n", d.Action, clipStr(d.Title, 120), clipStr(d.Answer, 200))
		}
	}
	approved, _ := s.st.Rules("approved")
	rejected, _ := s.st.Rules("rejected")
	list := func(rs []store.Rule) string {
		var b strings.Builder
		for _, r := range rs {
			fmt.Fprintf(&b, "- %s\n", r.Text)
		}
		return firstNonEmptyStr(b.String(), "(none)\n")
	}
	prompt := fmt.Sprintf(`You maintain the short list of working preferences that %[1]s's AI coding agents follow. Below are %[1]s's recent corrections of agents and decisions in their queue.

Propose at most 5 new rules, or refinements of existing ones, that would have prevented these corrections or that capture how %[1]s decides. Each rule must be supported by at least two items below; quote them as evidence. Rules are short, concrete and useful to an agent mid-task ("Prefer X over Y when Z"). Scope is "all" or a project name. Propose nothing that duplicates or contradicts the lists below; an empty list is fine.

Approved rules:
%[2]s
Rejected before (don't propose again):
%[3]s
Recent corrections and decisions:
%[4]s`, s.cfg.Person, list(approved), list(rejected), ev.String())
	raw, err := s.ask(ctx, s.chiefDefaults().RulesModel, prompt, rulesSchema)
	if err != nil {
		return 0, err
	}
	var res struct {
		Rules []struct {
			Text     string   `json:"text"`
			Scope    string   `json:"scope"`
			Evidence []string `json:"evidence"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range res.Rules {
		text := strings.TrimSpace(r.Text)
		if text == "" || len(r.Evidence) < 2 {
			continue
		}
		key := "rule:" + strings.ToLower(text)
		if s.st.EverFiled(key) {
			continue // proposed before: approved (maybe reworded), declined or open
		}
		body := "Proposed preference rule (scope: " + firstNonEmptyStr(r.Scope, "all") + "). Approve it, edit it by answering with your wording, or decline.\n\nEvidence:\n- " + strings.Join(r.Evidence, "\n- ")
		if _, isNew, err := s.st.UpsertItem(store.Item{ID: ulid.ItemID(), Kind: "rule", Source: ingest.SrcWatchdog, Title: clipStr(text, 300), Body: body,
			Blocks: firstNonEmptyStr(r.Scope, "all"), DedupeKey: key}); err == nil && isNew {
			n++
		}
	}
	s.st.Bump()
	return n, nil
}

func (s *Server) rulesTick(ctx context.Context) {
	every, err := parseEvery(s.chiefDefaults().RulesEvery)
	if err != nil || every <= 0 {
		return
	}
	last, _ := strconv.ParseInt(s.st.Get("brain:last"), 10, 64)
	now := s.cfg.Now()
	if time.Since(time.Unix(last, 0)) < every || now.Hour() < 10 {
		return
	}
	s.st.Set("brain:last", strconv.FormatInt(now.Unix(), 10))
	if n, err := s.ProposeRules(ctx); err != nil {
		s.log.Printf("rules: %v", err)
	} else if n > 0 {
		s.log.Printf("rules: proposed %d", n)
	}
}

func parseEvery(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		return time.Duration(n) * 24 * time.Hour, err
	}
	return time.ParseDuration(s)
}

// decideRule applies the human's decision on a proposed rule.
func (s *Server) decideRule(it store.Item, a Action) error {
	scope := firstNonEmptyStr(it.Blocks, "all")
	switch a.Kind {
	case "approve", "answer":
		text := it.Title
		if a.Kind == "answer" {
			text = strings.TrimSpace(a.Text)
			if text == "" {
				return errors.New("the rule text is empty")
			}
		}
		if err := s.st.SetRule(store.Rule{ID: it.ID, Text: text, Scope: scope, Status: "approved", Evidence: it.Body}); err != nil {
			return err
		}
		if err := s.st.SetStatus(it.ID, "done", "rule approved", "approve"); err != nil {
			return err
		}
	case "decline":
		if err := s.st.SetRule(store.Rule{ID: it.ID, Text: it.Title, Scope: scope, Status: "rejected", Evidence: it.Body}); err != nil {
			return err
		}
		if err := s.st.SetStatus(it.ID, "dismissed", "rule rejected", "decline"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("a rule can be approved, edited (answer with your wording) or declined")
	}
	s.rulesChanged()
	return nil
}

// RetireRule stops an approved rule from being sent to agents.
func (s *Server) RetireRule(id string) error {
	rs, err := s.st.Rules("")
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.ID == id {
			r.Status = "retired"
			if err := s.st.SetRule(r); err != nil {
				return err
			}
			s.rulesChanged()
			return nil
		}
	}
	return errors.New("no such rule")
}

func (s *Server) rulesChanged() {
	v, _ := strconv.Atoi(s.st.Get("rules:version"))
	s.st.Set("rules:version", strconv.Itoa(v+1))
	s.st.Bump()
}

// rulesText is the approved rules as a bullet list.
func (s *Server) rulesText() string {
	rs, _ := s.st.Rules("approved")
	var b strings.Builder
	for _, r := range rs {
		if r.Scope != "" && r.Scope != "all" {
			fmt.Fprintf(&b, "- (%s) %s\n", r.Scope, r.Text)
		} else {
			fmt.Fprintf(&b, "- %s\n", r.Text)
		}
	}
	return b.String()
}

// RulesMarkdown is ~/.docket/rules.md, imported by docket's CLAUDE.md block.
func (s *Server) RulesMarkdown() string {
	body := s.rulesText()
	if body == "" {
		body = "(none yet)\n"
	}
	return "# Preferences " + s.cfg.Person + " has approved\n\n" +
		"Managed by docket; " + s.cfg.Person + " approves these in the docket queue. Follow them unless a task says otherwise. A rule in parentheses applies only to that project.\n\n" + body
}

// syncRules sends changed rules to every machine the server can reach.
// Laptops get them with their next `docket sync`.
func (s *Server) syncRules(ctx context.Context) {
	v := s.st.Get("rules:version")
	if v == "" {
		return
	}
	md := s.RulesMarkdown()
	for _, m := range s.cfg.Machines {
		if m.Mode == "push" || s.st.Get("rules:sent:"+m.Name) == v {
			continue
		}
		if _, err := s.remote(ctx, m, strings.NewReader(md), "rules", "put"); err != nil {
			s.log.Printf("%s: rules: %v", m.Name, err)
			continue
		}
		s.st.Set("rules:sent:"+m.Name, v)
	}
}
