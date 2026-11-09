package ulid

import (
	"regexp"
	"testing"
	"time"
)

var valid = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func TestFormat(t *testing.T) {
	for i := 0; i < 1000; i++ {
		id := New(time.Now())
		if !valid.MatchString(id) {
			t.Fatalf("bad ULID %q", id)
		}
	}
}

func TestSortsByTime(t *testing.T) {
	a := New(time.UnixMilli(1_700_000_000_000))
	b := New(time.UnixMilli(1_700_000_000_001))
	if !(a < b) {
		t.Fatalf("expected %s < %s", a, b)
	}
}

func TestUnique(t *testing.T) {
	seen := map[string]bool{}
	now := time.Now()
	for i := 0; i < 10000; i++ {
		id := New(now)
		if seen[id] {
			t.Fatalf("duplicate %s", id)
		}
		seen[id] = true
	}
}

func TestItemID(t *testing.T) {
	re := regexp.MustCompile(`^dk-[0-9a-hjkmnp-tv-z]{8}$`)
	for i := 0; i < 100; i++ {
		if id := ItemID(); !re.MatchString(id) {
			t.Fatalf("bad item id %q", id)
		}
	}
}
