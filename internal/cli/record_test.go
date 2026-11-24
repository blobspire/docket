package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// A review says where its full version lives with --record; one filed
// without it gets a reminder that docket is a message, not the record.
func TestReviewRecordAndReminder(t *testing.T) {
	getenv := env(t)
	var out bytes.Buffer
	if err := File("review", []string{"Seed 1004: carry-over confirmed", "--project", "vlm"}, &out, getenv); err != nil {
		t.Fatal(err)
	}
	it := last(t).Item
	if it.Record != "" || !strings.Contains(out.String(), "no --record") || !strings.Contains(out.String(), "docket add "+it.ID+" --record") {
		t.Fatalf("no reminder for a review without --record:\n%s", out.String())
	}

	out.Reset()
	if err := File("review", []string{"Seed 1005 read", "--project", "vlm", "--record", "  results/lp/seed_1005 @ 3f2a1c9  "}, &out, getenv); err != nil {
		t.Fatal(err)
	}
	if it := last(t).Item; it.Record != "results/lp/seed_1005 @ 3f2a1c9" {
		t.Fatalf("record = %q", it.Record)
	}
	if strings.Contains(out.String(), "no --record") {
		t.Fatalf("reminder despite --record:\n%s", out.String())
	}

	// Only reviews are reminded: a question needs no record.
	out.Reset()
	if err := File("ask", []string{"Run the second seed?", "--project", "vlm"}, &out, getenv); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "--record") {
		t.Fatalf("an ask got the record reminder:\n%s", out.String())
	}

	if err := File("review", []string{"x", "--project", "vlm", "--record", "a\nb"}, &out, getenv); err == nil {
		t.Fatal("a two-line --record was accepted")
	}
	if err := File("review", []string{"x", "--project", "vlm", "--record", strings.Repeat("r", 501)}, &out, getenv); err == nil {
		t.Fatal("a 501-character --record was accepted")
	}
}

// docket add can say where the record is later, on its own.
func TestAddRecord(t *testing.T) {
	getenv := env(t)
	var out bytes.Buffer
	if err := Add([]string{"dk-abcdefgh", "--record", "https://github.com/o/r/pull/7"}, &out, getenv); err != nil {
		t.Fatal(err)
	}
	r := last(t)
	var p struct {
		ItemID string `json:"item_id"`
		Record string `json:"record"`
	}
	json.Unmarshal(r.Payload, &p)
	if r.Event != "cli.add" || p.ItemID != "dk-abcdefgh" || p.Record != "https://github.com/o/r/pull/7" {
		t.Fatalf("record %+v payload %+v", r, p)
	}
	if err := Add([]string{"dk-abcdefgh"}, &out, getenv); err == nil {
		t.Fatal("an empty add was accepted")
	}
}

// Re-filing with the key of an item that's still open updates it: the
// confirmation names the new filing's id (always valid: it's the new item,
// or an alias of the one the server merges it into), and docket list shows
// one entry under it.
func TestRefileWithKeyIsListedOnce(t *testing.T) {
	getenv := env(t)
	var out bytes.Buffer
	if err := File("review", []string{"Nightly results", "--project", "vlm", "--key", "nightly", "--record", "r1"}, &out, getenv); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := File("review", []string{"Nightly results, updated", "--project", "vlm", "--key", "nightly", "--record", "r2"}, &out, getenv); err != nil {
		t.Fatal(err)
	}
	second := last(t).Item.ID
	if !strings.Contains(out.String(), "docket: filed "+second) {
		t.Fatalf("re-file confirmation (second %s):\n%s", second, out.String())
	}
	var list bytes.Buffer
	if err := List([]string{"--all"}, &list, getenv); err != nil {
		t.Fatal(err)
	}
	if strings.Count(list.String(), "Nightly results") != 1 || !strings.Contains(list.String(), second) || !strings.Contains(list.String(), "Nightly results, updated") {
		t.Fatalf("docket list after a re-file:\n%s", list.String())
	}
}
