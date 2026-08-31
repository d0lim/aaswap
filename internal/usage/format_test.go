package usage

import (
	"testing"
	"time"
)

// A fixed local zone, so "same day" and the rendered clock are decided by the
// test rather than by wherever this runs.
var zone = time.FixedZone("TEST", 0)

func TestCountdown(t *testing.T) {
	now := time.Date(2026, 3, 23, 12, 0, 0, 0, zone)
	tests := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"days and hours", 2*24*time.Hour + 6*time.Hour, "2d 6h"},
		// The minutes are dropped once days appear: nobody schedules around
		// them two days out.
		{"days drop the minutes", 2*24*time.Hour + 6*time.Hour + 59*time.Minute, "2d 6h"},
		{"hours and minutes", 2*time.Hour + 15*time.Minute, "2h 15m"},
		{"minutes alone under an hour", 45 * time.Minute, "45m"},
		{"seconds round down", 90 * time.Second, "1m"},
		{"exactly now", 0, "0m"},
		// A reset already in the past reads as due, never as negative time.
		{"already past", -3 * time.Hour, "0m"},
		{"exactly one day", 24 * time.Hour, "1d 0h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Countdown(now.Add(tt.in), now); got != tt.want {
				t.Errorf("Countdown = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResetClock(t *testing.T) {
	now := time.Date(2026, 3, 23, 12, 0, 0, 0, zone)
	tests := []struct {
		name  string
		reset time.Time
		want  string
	}{
		{"same day is the time alone", now.Add(2 * time.Hour), "14:00"},
		{"a later day carries the date", now.Add(2 * 24 * time.Hour), "Mar 25 12:00"},
		// Two hours apart but across midnight: the date is what stops "01:00"
		// from reading as an hour ago.
		{"just past midnight carries the date", now.Add(13 * time.Hour), "Mar 24 01:00"},
		{"the day is unpadded", time.Date(2026, 7, 5, 8, 59, 0, 0, zone), "Jul 5 08:59"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResetClock(tt.reset, now); got != tt.want {
				t.Errorf("ResetClock = %q, want %q", got, tt.want)
			}
		})
	}
}

// The clock is rendered in now's location, so a reset is read against the
// reader's own day. The same instant can therefore be same-day for one reader
// and a dated tomorrow for another — which is exactly why the location has to
// come from now rather than from the timestamp.
func TestResetClockUsesNowsLocation(t *testing.T) {
	reset := time.Date(2026, 3, 24, 14, 0, 0, 0, time.UTC)
	now := time.Date(2026, 3, 23, 21, 0, 0, 0, time.UTC)
	tokyo := time.FixedZone("JST", 9*3600)

	// In UTC the reset is tomorrow, so the date has to be shown.
	if got := ResetClock(reset, now); got != "Mar 24 14:00" {
		t.Errorf("ResetClock in UTC = %q, want %q", got, "Mar 24 14:00")
	}
	// The same instant in Tokyo is 23:00 on the reader's own day.
	if got := ResetClock(reset, now.In(tokyo)); got != "23:00" {
		t.Errorf("ResetClock in Tokyo = %q, want %q", got, "23:00")
	}
}

func TestFormatReset(t *testing.T) {
	now := time.Date(2026, 3, 23, 12, 0, 0, 0, zone)

	countdown, clock, ok := FormatReset("2026-03-23T14:15:00Z", now)
	if !ok {
		t.Fatal("FormatReset rejected a valid timestamp")
	}
	if countdown != "2h 15m" || clock != "14:15" {
		t.Errorf("FormatReset = (%q, %q), want (%q, %q)", countdown, clock, "2h 15m", "14:15")
	}

	for _, in := range []string{"", "not a timestamp"} {
		if _, _, ok := FormatReset(in, now); ok {
			t.Errorf("FormatReset(%q) reported a result", in)
		}
	}
}
