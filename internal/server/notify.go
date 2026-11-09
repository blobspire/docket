package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Push priorities (ntfy: 1 min … 5 max).
const (
	prioLow    = 2
	prioHigh   = 4
	prioUrgent = 5
)

// push sends a phone notification through ntfy. It is a no-op when no topic
// is configured.
func (s *Server) push(ctx context.Context, title, body, click string, priority int, tags ...string) error {
	n := s.cfg.Ntfy
	if n.Topic == "" {
		return nil
	}
	msg := map[string]any{"topic": n.Topic, "title": title, "message": body, "priority": priority}
	if click != "" {
		msg["click"] = click
	}
	if len(tags) > 0 {
		msg["tags"] = tags
	}
	b, _ := json.Marshal(msg)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(n.Server, "/"), bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		e, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("ntfy: %s %s", resp.Status, strings.TrimSpace(string(e)))
	}
	return nil
}

func (s *Server) itemURL(id string) string {
	if s.cfg.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(s.cfg.PublicURL, "/") + "/item/" + id
}
