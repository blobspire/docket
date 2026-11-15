package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/config"
	"github.com/blobspire/docket/internal/mailbox"
	"github.com/blobspire/docket/internal/turnstate"
)

func envWith(base func(string) string, kv map[string]string) func(string) string {
	return func(k string) string {
		if v, ok := kv[k]; ok {
			return v
		}
		return base(k)
	}
}

func put(t *testing.T, sid, text string) {
	t.Helper()
	err := mailbox.Box{Dir: config.MailboxDir()}.Put(mailbox.Delivery{ID: "01D" + sid, ItemID: "dk-12345678", SessionID: sid, Nonce: "nonce1", Text: text})
	if err != nil {
		t.Fatal(err)
	}
}

func receiptCount(t *testing.T) int {
	n := 0
	for _, r := range records(t) {
		if r.Event == "docket.receipt" {
			n++
		}
	}
	return n
}

func TestPostToolUseDeliversWaitingAnswerOnce(t *testing.T) {
	getenv := setup(t)
	put(t, "s1", "[docket dk-12345678 #nonce1] Cole answered: ship it")
	var out bytes.Buffer
	Run("PostToolUse", strings.NewReader(`{"session_id":"s1","tool_name":"Bash"}`), &out, &bytes.Buffer{}, getenv)
	var resp struct {
		H struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil || resp.H.Event != "PostToolUse" || !strings.Contains(resp.H.Context, "ship it") {
		t.Fatalf("output %q (%v)", out.String(), err)
	}
	if receiptCount(t) != 1 {
		t.Fatal("no receipt recorded")
	}
	out.Reset()
	Run("PostToolUse", strings.NewReader(`{"session_id":"s1"}`), &out, &bytes.Buffer{}, getenv)
	if out.Len() != 0 {
		t.Fatal("delivered twice")
	}
}

func TestPostToolUseNeverDeliversIntoSubagents(t *testing.T) {
	getenv := setup(t)
	put(t, "s1", "answer")
	var out bytes.Buffer
	Run("PostToolUse", strings.NewReader(`{"session_id":"s1","agent_id":"sub-1"}`), &out, &bytes.Buffer{}, getenv)
	if out.Len() != 0 {
		t.Fatal("delivered into a subagent")
	}
}

func TestPostToolUseRecordsNothingWhenIdle(t *testing.T) {
	getenv := setup(t)
	Run("PostToolUse", strings.NewReader(`{"session_id":"s1"}`), &bytes.Buffer{}, &bytes.Buffer{}, getenv)
	if n := len(records(t)); n != 0 {
		t.Fatalf("recorded %d events on the hot path", n)
	}
}

func TestWaiterWakesSessionWithAnswer(t *testing.T) {
	getenv := envWith(setup(t), map[string]string{"DOCKET_WAIT_POLL": "20ms", "DOCKET_WAIT_MAX": "5s", "CLAUDE_PID": "1"})
	go func() {
		time.Sleep(100 * time.Millisecond)
		put(t, "s2", "[docket dk-12345678 #nonce1] Cole answered: use option B")
	}()
	var stderr bytes.Buffer
	code, _ := Run("Wait", strings.NewReader(`{"session_id":"s2"}`), &bytes.Buffer{}, &stderr, getenv)
	if code != 2 || !strings.Contains(stderr.String(), "option B") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestWaiterStepsAsideForNewerWaiter(t *testing.T) {
	getenv := envWith(setup(t), map[string]string{"DOCKET_WAIT_POLL": "20ms", "DOCKET_WAIT_MAX": "5s"})
	done := make(chan int)
	go func() {
		code, _ := Run("Wait", strings.NewReader(`{"session_id":"s3"}`), &bytes.Buffer{}, &bytes.Buffer{}, getenv)
		done <- code
	}()
	time.Sleep(100 * time.Millisecond)
	st, _ := turnstate.For("s3")
	st.SetWaiter(os.Getpid() + 1) // a newer turn's waiter
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("superseded waiter exited %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("superseded waiter kept running")
	}
}

func TestWaiterExitsWhenSessionDies(t *testing.T) {
	getenv := envWith(setup(t), map[string]string{"DOCKET_WAIT_POLL": "20ms", "DOCKET_WAIT_MAX": "5s", "CLAUDE_PID": "999999"})
	start := time.Now()
	code, _ := Run("Wait", strings.NewReader(`{"session_id":"s4"}`), &bytes.Buffer{}, &bytes.Buffer{}, getenv)
	if code != 0 || time.Since(start) > time.Second {
		t.Fatalf("code=%d after %v", code, time.Since(start))
	}
}

func stop(t *testing.T, getenv func(string) string, sid, msg string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"session_id": sid, "hook_event_name": "Stop", "last_assistant_message": msg})
	var out bytes.Buffer
	Run("Stop", bytes.NewReader(payload), &out, &bytes.Buffer{}, getenv)
	return out.String()
}

func TestStopNeverNudges(t *testing.T) {
	getenv := setup(t)
	if out := stop(t, getenv, "s8", "It needs your approval before I deploy."); out != "" {
		t.Fatalf("Stop printed something with nothing to deliver: %q", out)
	}
	for _, r := range records(t) {
		if r.Event == "docket.nudge" {
			t.Fatal("a nudge record was written")
		}
	}
}

func TestQuietDeliveryNeverWakes(t *testing.T) {
	getenv := envWith(setup(t), map[string]string{"DOCKET_WAIT_POLL": "20ms", "DOCKET_WAIT_MAX": "300ms"})
	box := mailbox.Box{Dir: config.MailboxDir()}
	if err := box.Put(mailbox.Delivery{ID: "q1", SessionID: "s10", Nonce: "n1", Text: "[docket dk-12345678 #n1] feedback", Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if code, _ := Run("Wait", strings.NewReader(`{"session_id":"s10"}`), &bytes.Buffer{}, &bytes.Buffer{}, getenv); code != 0 {
		t.Fatalf("a quiet delivery woke the session (code %d)", code)
	}
	if out := stop(t, getenv, "s10", "done"); !strings.Contains(out, "feedback") {
		t.Fatalf("the next turn didn't deliver it: %q", out)
	}
}

func TestStopDeliversAnswer(t *testing.T) {
	getenv := setup(t)
	put(t, "s9", "[docket dk-12345678 #nonce1] Cole answered: approved")
	out := stop(t, getenv, "s9", "Waiting on your approval.")
	if !strings.Contains(out, "Cole answered: approved") {
		t.Fatalf("output %q", out)
	}
}
