package provider

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeRollout(t *testing.T, home, day, name string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", day)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The record Codex writes after every turn, in the shape it actually writes:
// every rollout line is {timestamp, type, payload}, and the limits are inside
// the payload. Copied from a real session rather than reconstructed — an
// invented fixture is how a wrong struct passes its own tests.
const rateLimitLine = `{"timestamp":"2026-08-18T13:06:00Z","type":"token_count","payload":{` +
	`"type":"token_count","info":{"total_token_usage":{"total_tokens":19564}},` +
	`"rate_limits":{"limit_id":"codex","plan_type":"plus",` +
	`"primary":{"used_percent":16,"window_minutes":10080,"resets_at":1787617468},` +
	`"secondary":{"used_percent":42.5,"window_minutes":300,"resets_at":1787600000},` +
	`"credits":{"has_credits":false,"unlimited":false,"balance":"0"}}}}`

// aaswap does not ask OpenAI what a Codex account has left — Codex already
// asked, on every turn, and wrote the answer down. Reading that costs no
// request and consumes no quota.
func TestCodexUsageIsReadFromWhatCodexAlreadyRecorded(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, "2026/08/18", "rollout-a.jsonl",
		`{"type":"session_meta","payload":{"id":"x"}}`, rateLimitLine)

	got, at, ok := CodexUsage(home)
	if !ok {
		t.Fatal("no measurement was read")
	}
	// The windows are matched by their own length, not by position: primary
	// and secondary are whichever two the plan has, and a plan with one leaves
	// the other null.
	if got.SevenDay == nil || got.SevenDay.Pct != 16 {
		t.Errorf("sevenDay = %+v, want the 10080-minute window", got.SevenDay)
	}
	if got.FiveHour == nil || got.FiveHour.Pct != 42.5 {
		t.Errorf("fiveHour = %+v, want the 300-minute window", got.FiveHour)
	}
	// Unix seconds on the wire, RFC3339 in the model, because every other
	// reader of this field parses it that way.
	if want := time.Unix(1787617468, 0).UTC().Format(time.RFC3339); got.SevenDay.ResetsAt != want {
		t.Errorf("resetsAt = %q, want %q", got.SevenDay.ResetsAt, want)
	}
	if at.IsZero() {
		t.Error("the measurement carries no time, so nothing can judge its age")
	}
}

// The newest record wins. An old session's numbers describe a window that has
// probably reset since.
func TestCodexUsageTakesTheNewestRecord(t *testing.T) {
	home := t.TempDir()
	old := writeRollout(t, home, "2026/01/01", "rollout-old.jsonl",
		`{"timestamp":"2026-01-01T00:00:00Z","type":"token_count","payload":{`+
			`"rate_limits":{"primary":{"used_percent":99,"window_minutes":10080}}}}`)
	recent := writeRollout(t, home, "2026/08/18", "rollout-new.jsonl", rateLimitLine)

	// Make the ordering unambiguous regardless of how the fixture landed.
	past := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(recent, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}

	got, _, ok := CodexUsage(home)
	if !ok {
		t.Fatal("no measurement was read")
	}
	if got.SevenDay.Pct != 16 {
		t.Errorf("pct = %v, want the newest session's 16", got.SevenDay.Pct)
	}
}

// A machine that has never run Codex, and one whose sessions carry no limits
// yet, both report nothing rather than zero. Zero would read as "plenty left".
func TestCodexUsageReportsNothingRatherThanZero(t *testing.T) {
	t.Run("no sessions at all", func(t *testing.T) {
		if _, _, ok := CodexUsage(t.TempDir()); ok {
			t.Error("an empty install reported a measurement")
		}
	})
	t.Run("sessions with no limits recorded", func(t *testing.T) {
		home := t.TempDir()
		writeRollout(t, home, "2026/08/18", "rollout-a.jsonl",
			`{"type":"session_meta","payload":{"id":"x"}}`,
			`{"type":"event_msg","payload":{"type":"task_started"}}`)
		if _, _, ok := CodexUsage(home); ok {
			t.Error("a session with no limits reported a measurement")
		}
	})
	t.Run("a torn line is skipped, not fatal", func(t *testing.T) {
		home := t.TempDir()
		writeRollout(t, home, "2026/08/18", "rollout-a.jsonl",
			`{"type":"token_count","payload":{"rate_limits":`, rateLimitLine)
		if _, _, ok := CodexUsage(home); !ok {
			t.Error("one torn line lost the whole file")
		}
	})
}
