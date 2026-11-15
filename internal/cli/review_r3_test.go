package cli

import (
	"bytes"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/agentctx"
)

// done records its record id, so the overlay defers once the server applies it.
func TestDoneKeepsItsRecordID(t *testing.T) {
	getenv := env(t)
	writeContext(t, agentctx.Context{AsOf: time.Now().Unix(), Projects: []agentctx.Project{{Name: "windward"}}})
	if err := Done([]string{"dk-12345678"}, &bytes.Buffer{}, getenv); err != nil {
		t.Fatal(err)
	}
	o := agentctx.LoadOverlay()
	if len(o.Closed) != 1 || o.Closed[0].RecordID == "" || o.Closed[0].RecordID != last(t).ID {
		t.Fatalf("closed = %+v", o.Closed)
	}
}

// done --key with a blank key is refused.
func TestDoneBlankKey(t *testing.T) {
	getenv := env(t)
	if err := Done([]string{"--key", "   "}, &bytes.Buffer{}, getenv); err == nil {
		t.Fatal("accepted a blank key")
	}
}

// done --key hides exactly the item the server will close.
func TestDoneByKeyHidesOneItem(t *testing.T) {
	getenv := env(t)
	now := time.Now().Unix()
	writeContext(t, agentctx.Context{AsOf: now, Projects: []agentctx.Project{{Name: "windward"}, {Name: "cwant"}}, Open: []agentctx.Item{
		{ID: "dk-aaaaaaaa", Title: "A", Project: "windward", Key: "ci", CreatedAt: now - 60},
		{ID: "dk-bbbbbbbb", Title: "B", Project: "cwant", Key: "ci", CreatedAt: now - 30},
	}})
	if err := Done([]string{"--key", "ci", "--project", "windward"}, &bytes.Buffer{}, getenv); err != nil {
		t.Fatal(err)
	}
	o := agentctx.LoadOverlay()
	if len(o.Closed) != 1 || o.Closed[0].ID != "dk-aaaaaaaa" {
		t.Fatalf("closed = %+v", o.Closed)
	}
}
