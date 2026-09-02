package provider

import (
	json "encoding/json/v2"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/d0lim/aaswap/internal/usage"
)

// shortWindowCutoff separates the two rate-limit windows a plan carries.
//
// Codex names them "primary" and "secondary" rather than by length, and which
// is which varies by plan. Sorting by the window's OWN length is the only
// mapping that does not depend on that ordering: anything a day or shorter is
// the rolling window, anything longer is the weekly one.
const shortWindowCutoff = 24 * 60

// rolloutScanLimit caps how many session files are opened looking for a
// measurement.
//
// A long-lived install accumulates thousands. The newest few hold anything
// recent enough to be worth showing, and a listing must not turn into a
// directory walk of a year of sessions.
const rolloutScanLimit = 40

// CodexUsage reads an account's rate limits out of what Codex already
// recorded, and reports when it was recorded.
//
// # Why this is not an API call
//
// Codex receives its rate limits in the response to every turn it takes, and
// writes them into the session rollout it is already keeping. Reading that
// costs no request, consumes no quota, and cannot be throttled — where asking
// OpenAI directly would do all three, and `/backend-api/api/codex/usage` sits
// behind a challenge a plain HTTP client does not pass anyway.
//
// # What it therefore cannot do
//
// The rollout does not record WHICH account was signed in, so a measurement
// can only be attributed to whoever is live now. An idle Codex account reports
// nothing rather than stale numbers, and nothing is the honest answer: the
// windows it last had have probably reset since.
//
// Reports false rather than a zero measurement when there is nothing to read.
// Zero would render as "plenty left" and could send a person onto an
// account that is actually spent.
func CodexUsage(codexHome string) (*usage.Result, time.Time, bool) {
	for _, path := range recentRollouts(filepath.Join(codexHome, "sessions")) {
		if result, at, ok := usageFromRollout(path); ok {
			return result, at, true
		}
	}
	return nil, time.Time{}, false
}

// recentRollouts lists session files newest-first, capped.
func recentRollouts(sessions string) []string {
	type entry struct {
		path string
		mod  time.Time
	}
	var found []entry
	// Collect what can be read and walk past what cannot. An unreadable
	// subtree, or a file that vanished mid-walk, must not hide every session
	// behind it — this is a display convenience, not a store read.
	_ = filepath.WalkDir(sessions, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
			if info, statErr := d.Info(); statErr == nil {
				found = append(found, entry{path: path, mod: info.ModTime()})
			}
		}
		return nil
	})
	sort.Slice(found, func(i, j int) bool { return found[i].mod.After(found[j].mod) })
	paths := make([]string, 0, min(len(found), rolloutScanLimit))
	for _, e := range found[:min(len(found), rolloutScanLimit)] {
		paths = append(paths, e.path)
	}
	return paths
}

// rolloutRecord is the one line shape this cares about.
//
// The limits sit under `payload`, not at the top level: every rollout line is
// {timestamp, type, payload}, and the payload is what varies by type.
type rolloutRecord struct {
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Limits *struct {
			Primary   *codexWindow `json:"primary"`
			Secondary *codexWindow `json:"secondary"`
			PlanType  string       `json:"plan_type"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

type codexWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

// usageFromRollout returns the LAST rate-limit record in one session file.
//
// Last rather than first: a session writes one per turn, and the final one is
// the state the session ended in.
func usageFromRollout(path string) (*usage.Result, time.Time, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, false
	}
	var newest rolloutRecord
	var found bool
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.Contains(line, `"rate_limits"`) {
			continue
		}
		var record rolloutRecord
		// A torn line — a session killed mid-write — is skipped rather than
		// fatal: the lines before it are intact and are what this is after.
		if err := json.Unmarshal([]byte(line), &record); err != nil || record.Payload.Limits == nil {
			continue
		}
		newest, found = record, true
	}
	if !found {
		return nil, time.Time{}, false
	}

	result := &usage.Result{}
	for _, window := range []*codexWindow{newest.Payload.Limits.Primary, newest.Payload.Limits.Secondary} {
		if window == nil {
			continue
		}
		converted := &usage.Window{Pct: window.UsedPercent}
		if window.ResetsAt > 0 {
			converted.ResetsAt = time.Unix(window.ResetsAt, 0).UTC().Format(time.RFC3339)
		}
		if window.WindowMinutes <= shortWindowCutoff {
			result.FiveHour = converted
		} else {
			result.SevenDay = converted
		}
	}
	if result.Empty() {
		return nil, time.Time{}, false
	}

	at, err := time.Parse(time.RFC3339, newest.Timestamp)
	if err != nil {
		// The record is real even when its stamp is not; the file's own time is
		// the next-best answer, and an age is only used to caveat a display.
		if info, statErr := os.Stat(path); statErr == nil {
			at = info.ModTime()
		} else {
			at = time.Now()
		}
	}
	return result, at, true
}
