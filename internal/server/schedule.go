package server

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A schedule says when a lead is woken. Forms:
//
//	hourly          at minute 0 of every hour
//	hourly@47       at minute 47 of every hour
//	daily@09:00     once a day at 09:00 (server time zone)
//	every 30m       every 30 minutes (any Go duration of at least 1m)
type schedule struct {
	kind     string // hourly | daily | every
	minute   int
	hour     int
	interval time.Duration
}

func parseSchedule(s string) (schedule, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch {
	case s == "hourly":
		return schedule{kind: "hourly"}, nil
	case strings.HasPrefix(s, "hourly@"):
		m, err := strconv.Atoi(strings.TrimPrefix(s, "hourly@"))
		if err != nil || m < 0 || m > 59 {
			return schedule{}, fmt.Errorf("schedule %q: minute must be 0-59", s)
		}
		return schedule{kind: "hourly", minute: m}, nil
	case strings.HasPrefix(s, "daily@"):
		mm := minutes(strings.TrimPrefix(s, "daily@"))
		if mm < 0 {
			return schedule{}, fmt.Errorf("schedule %q: use daily@HH:MM", s)
		}
		return schedule{kind: "daily", hour: mm / 60, minute: mm % 60}, nil
	case strings.HasPrefix(s, "every "):
		d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(s, "every ")))
		if err != nil || d < time.Minute {
			return schedule{}, fmt.Errorf("schedule %q: use e.g. \"every 30m\" (at least 1m)", s)
		}
		return schedule{kind: "every", interval: d}, nil
	}
	return schedule{}, fmt.Errorf("unknown schedule %q (use hourly, hourly@MM, daily@HH:MM or \"every <duration>\")", s)
}

// due reports whether a wake should fire at now, given the last fire time.
// A run missed while the server was down fires once on the next tick.
func (sc schedule) due(last, now time.Time) bool {
	if last.IsZero() {
		last = now.Add(-time.Second) // first run: wait for the next slot
		if sc.kind == "every" {
			return true
		}
	}
	return !sc.next(last).After(now)
}

// next returns the first fire time strictly after t.
func (sc schedule) next(t time.Time) time.Time {
	switch sc.kind {
	case "every":
		return t.Add(sc.interval)
	case "daily":
		n := time.Date(t.Year(), t.Month(), t.Day(), sc.hour, sc.minute, 0, 0, t.Location())
		if !n.After(t) {
			n = n.AddDate(0, 0, 1)
		}
		return n
	default:
		n := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), sc.minute, 0, 0, t.Location())
		if !n.After(t) {
			n = n.Add(time.Hour)
		}
		return n
	}
}
