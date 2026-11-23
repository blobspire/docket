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
)

// modelCall is one headless Claude Code run for email triage or drafting.
//
// Every run starts in safe mode (no CLAUDE.md, skills, plugins, hooks or
// MCP servers) with --strict-mcp-config and an explicit tool list, and
// takes its prompt on stdin. A drafting run adds --restricted with
// Read,Grep,Glob only: it can read the project's code in its working
// directory and nothing else. No run ever has a shell, the web, or mail.
type modelCall struct {
	Model      string
	Prompt     string
	Schema     string
	Tools      string // "" for none, or "Read,Grep,Glob"
	Dir        string // working directory ("" = an empty temp dir)
	Restricted bool
	Resume     string // continue this session
	Keep       bool   // keep the session, so it can be resumed
	Budget     float64
	Timeout    time.Duration
}

type modelResult struct {
	Out     json.RawMessage
	Session string
	Cost    float64
}

// allowedTools is every tool list a model call may get. Anything else is a
// programming error, refused before claude runs.
var allowedTools = map[string]bool{"": true, "Read,Grep,Glob": true}

func (c modelCall) args() ([]string, error) {
	if !allowedTools[c.Tools] {
		return nil, fmt.Errorf("model call: tools %q not allowed", c.Tools)
	}
	if c.Tools != "" && !c.Restricted {
		return nil, errors.New("model call: a run with tools must be restricted")
	}
	args := []string{"-p", "--model", c.Model, "--safe-mode", "--strict-mcp-config", "--tools", c.Tools,
		"--output-format", "json", "--json-schema", c.Schema, "--max-budget-usd", strconv.FormatFloat(c.Budget, 'f', 2, 64)}
	if c.Restricted {
		args = append(args, "--restricted")
	}
	if !c.Keep {
		args = append(args, "--no-session-persistence")
	}
	if c.Resume != "" {
		args = append(args, "--resume", c.Resume)
	}
	return args, nil
}

func (s *Server) runModel(ctx context.Context, c modelCall) (modelResult, error) {
	args, err := c.args()
	if err != nil {
		return modelResult{}, err
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	dir := c.Dir
	if dir == "" {
		d, err := os.MkdirTemp("", "docket-email-")
		if err != nil {
			return modelResult{}, err
		}
		defer os.RemoveAll(d)
		dir = d
	}
	cmd := exec.CommandContext(ctx, s.chiefDefaults().Claude, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DOCKET_INTERNAL=1")
	cmd.Stdin = strings.NewReader(c.Prompt)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	var res struct {
		IsError    bool            `json:"is_error"`
		Result     string          `json:"result"`
		Structured json.RawMessage `json:"structured_output"`
		Cost       float64         `json:"total_cost_usd"`
		Session    string          `json:"session_id"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		if runErr != nil {
			return modelResult{}, fmt.Errorf("claude: %v %s", runErr, clipErr(errb.String()))
		}
		return modelResult{}, fmt.Errorf("claude output: %w", err)
	}
	r := modelResult{Session: res.Session, Cost: res.Cost}
	if res.IsError {
		return r, errors.New("claude: " + clipErr(res.Result))
	}
	if runErr != nil {
		return r, fmt.Errorf("claude: %v %s", runErr, clipErr(errb.String()))
	}
	if len(res.Structured) > 0 && string(res.Structured) != "null" {
		r.Out = res.Structured
	} else {
		r.Out = json.RawMessage(res.Result)
	}
	return r, nil
}
