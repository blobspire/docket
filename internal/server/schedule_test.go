package server

import (
	"testing"
	"time"
)

func TestSchedules(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 9, h, m, 0, 0, time.UTC) }
	cases := []struct {
		spec      string
		last, now time.Time
		want      bool
	}{
		{"hourly@47", at(9, 47), at(10, 46), false},
		{"hourly@47", at(9, 47), at(10, 47), true},
		{"hourly@47", at(9, 47), at(13, 5), true}, // missed while down: fire once
		{"hourly", at(10, 0), at(10, 30), false},
		{"daily@09:00", at(9, 0), at(23, 0), false},
		{"daily@09:00", at(9, 0).AddDate(0, 0, -1), at(9, 1), true},
		{"every 30m", at(10, 0), at(10, 29), false},
		{"every 30m", at(10, 0), at(10, 30), true},
		{"every 30m", time.Time{}, at(10, 0), true},
		{"hourly@15", time.Time{}, at(10, 10), false}, // first run waits for the slot
	}
	for _, c := range cases {
		sc, err := parseSchedule(c.spec)
		if err != nil {
			t.Fatal(err)
		}
		if got := sc.due(c.last, c.now); got != c.want {
			t.Errorf("%s last=%v now=%v: due=%v want %v", c.spec, c.last.Format("15:04"), c.now.Format("Jan2 15:04"), got, c.want)
		}
	}
	for _, bad := range []string{"hourly@60", "daily@25:00", "every 10s", "weekly"} {
		if _, err := parseSchedule(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestRecommendedOption(t *testing.T) {
	cases := []struct {
		opt, rec string
		want     bool
	}{
		{"a: also run the base checkpoint on B200", "a", true},
		{"deploy", "deploy", true},
		{"Deploy now", "deploy now", true},
		{"b: no extra run", "a", false},
		{"abort", "a", false},
		{"wait", "", false},
		{"2) keep going", "2", true},
	}
	for _, c := range cases {
		if got := recommended(c.opt, c.rec); got != c.want {
			t.Errorf("recommended(%q, %q) = %v", c.opt, c.rec, got)
		}
	}
}
