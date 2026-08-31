package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/realiti4/claude-swap/internal/claudeapi"
	"github.com/realiti4/claude-swap/internal/usage"
	"github.com/realiti4/claude-swap/internal/usagestore"
)

var now = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

func plain() *Printer {
	// Color off: the tests are about the words, and escape codes would make
	// every assertion a substring search through them.
	return &Printer{Out: &bytes.Buffer{}, Color: false, Theme: Dark}
}

// Piped output is read by programs, and escape codes there are corruption.
func TestColorIsOffForANonTerminal(t *testing.T) {
	var buf bytes.Buffer
	if colorEnabled(&buf) {
		t.Error("color was enabled for a buffer")
	}
}

func TestStylesAreNoOpsWithoutColor(t *testing.T) {
	p := plain()
	for name, fn := range map[string]func(string) string{
		"Accent": p.Accent, "Muted": p.Muted, "Dimmed": p.Dimmed,
		"Bold": p.Bold, "Red": p.Red, "Yellow": p.Yellow,
	} {
		if got := fn("text"); got != "text" {
			t.Errorf("%s = %q, want the text unchanged", name, got)
		}
	}
}

func TestStylesWrapWhenColorIsOn(t *testing.T) {
	p := &Printer{Out: &bytes.Buffer{}, Color: true, Theme: Dark}
	got := p.Accent("text")
	if !strings.HasPrefix(got, "\033[") || !strings.HasSuffix(got, reset) {
		t.Errorf("Accent = %q, want it wrapped in escape codes", got)
	}
	// Empty text is left alone: wrapping nothing emits codes with no content,
	// which shows up as stray bytes in a log.
	if got := p.Accent(""); got != "" {
		t.Errorf("Accent(\"\") = %q, want empty", got)
	}
}

// The light palette is not the dark one dimmed: the dark palette's mid-tones
// vanish on white, so every color differs.
func TestTheThemesShareNoColors(t *testing.T) {
	dark, light := palettes[Dark], palettes[Light]
	for name, pair := range map[string][2]string{
		"accent": {dark.accent, light.accent},
		"muted":  {dark.muted, light.muted},
		"red":    {dark.red, light.red},
		"yellow": {dark.yellow, light.yellow},
	} {
		if pair[0] == pair[1] {
			t.Errorf("%s is identical in both themes", name)
		}
	}
}

func TestUsageLines(t *testing.T) {
	reset := now.Add(3 * time.Hour).Format(time.RFC3339)
	result := &usage.Result{
		FiveHour: &usage.Window{Pct: 22, ResetsAt: reset},
		SevenDay: &usage.Window{Pct: 61, ResetsAt: now.Add(48 * time.Hour).Format(time.RFC3339)},
		Spend:    &usage.Spend{Used: 729, Limit: 5000, Pct: 14.58, Currency: "USD"},
		Scoped:   []usage.Scoped{{Name: "Fable", Pct: 100, ResetsAt: reset}},
	}
	lines := UsageLines(result, now, now)
	joined := strings.Join(lines, "\n")

	for _, want := range []string{"$$:", "5h:", "7d:", "Fable:", "22%", "61%", "100%"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the rendering is missing %q:\n%s", want, joined)
		}
	}
	// Thousands separators, or a five-figure spend is unreadable.
	if !strings.Contains(joined, "$729.00") || !strings.Contains(joined, "$5,000.00") {
		t.Errorf("the spend line does not group thousands:\n%s", joined)
	}
	// A model at its limit is flagged: it is the usual reason to switch.
	if !strings.Contains(joined, "(!)") {
		t.Errorf("a maxed model was not flagged:\n%s", joined)
	}
	// Labels are padded to the widest, so a per-model name does not shift the
	// columns of the account-wide lines beside it.
	for _, line := range lines[1:] {
		if strings.Index(line, "%") != strings.Index(lines[0], "%") {
			t.Errorf("the percentage columns are not aligned:\n%s", joined)
			break
		}
	}
}

// The reset strings are recomputed, not cached: a countdown frozen at fetch
// time overstates the remaining wait by however long the measurement sat.
func TestTheCountdownFollowsTheClock(t *testing.T) {
	reset := now.Add(3 * time.Hour).Format(time.RFC3339)
	result := &usage.Result{SevenDay: &usage.Window{Pct: 40, ResetsAt: reset}}

	atFetch := strings.Join(UsageLines(result, now, now), "")
	twoHoursLater := strings.Join(UsageLines(result, now, now.Add(2*time.Hour)), "")

	if !strings.Contains(atFetch, "in 3h 0m") {
		t.Errorf("at fetch time: %s", atFetch)
	}
	if !strings.Contains(twoHoursLater, "in 1h 0m") {
		t.Errorf("two hours later: %s", twoHoursLater)
	}
}

// A window with no reset still reports its percentage: the percentage is what
// gates the account.
func TestAWindowWithNoResetStillRenders(t *testing.T) {
	lines := UsageLines(&usage.Result{SevenDay: &usage.Window{Pct: 61}}, now, now)
	if len(lines) != 1 || !strings.Contains(lines[0], "61%") {
		t.Errorf("lines = %v", lines)
	}
	if strings.Contains(lines[0], "resets") {
		t.Errorf("a window with no reset claimed one: %q", lines[0])
	}
}

func TestEntryLines(t *testing.T) {
	measurement := &usage.Result{SevenDay: &usage.Window{Pct: 61}}

	tests := []struct {
		name  string
		entry usagestore.Entry
		want  []string
		avoid []string
	}{
		{
			name:  "a measurement",
			entry: usagestore.Entry{LastGood: measurement, FetchedAt: now, Age: time.Minute},
			want:  []string{"61%"},
		},
		{
			// A served measurement older than the poll floor carries its age,
			// so the user knows they are looking at something kept rather than
			// something just measured.
			name:  "an older measurement carries its age",
			entry: usagestore.Entry{LastGood: measurement, FetchedAt: now, Age: 20 * time.Minute},
			want:  []string{"61%", "20m ago"},
		},
		{
			name:  "a sentinel explains itself",
			entry: usagestore.Entry{Sentinel: "re-login needed"},
			want:  []string{"re-login needed", "ccswap add"},
		},
		{
			// The state says why there is no number; the older number is still
			// worth showing.
			name: "a sentinel with an older measurement",
			entry: usagestore.Entry{
				Sentinel: "token expired", LastGood: measurement,
				FetchedAt: now, Age: 20 * time.Minute,
			},
			want: []string{"token expired", "last seen 61% used", "20m ago"},
		},
		{
			// An API-key account has no quota to have last seen.
			name: "an API-key account shows no last-seen line",
			entry: usagestore.Entry{
				Sentinel: "api key", LastGood: measurement, FetchedAt: now, Age: time.Minute,
			},
			want:  []string{"API key"},
			avoid: []string{"last seen"},
		},
		{
			// A failing endpoint must be visible, not a silent blank.
			name:  "no measurement at all names the failure",
			entry: usagestore.Entry{LastError: claudeapi.HTTPKind(429)},
			want:  []string{"usage unavailable", "http-429"},
		},
		{
			name:  "an error with a remedy shows it",
			entry: usagestore.Entry{LastError: claudeapi.KindConsumeBusy},
			want:  []string{"usage unavailable", "another ccswap surface"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			joined := strings.Join(EntryLines(tt.entry, now, 5*time.Minute), "\n")
			for _, want := range tt.want {
				if !strings.Contains(joined, want) {
					t.Errorf("missing %q:\n%s", want, joined)
				}
			}
			for _, avoid := range tt.avoid {
				if strings.Contains(joined, avoid) {
					t.Errorf("unexpectedly contains %q:\n%s", avoid, joined)
				}
			}
		})
	}
}

// An owned-and-expired token means Claude Code will refresh it, NOT that the
// user must log in again. A note that gets that backwards sends people to
// re-authenticate for nothing.
func TestSentinelNotesDistinguishWhoMustAct(t *testing.T) {
	expired := SentinelNote("token expired")
	if !strings.Contains(expired, "retries automatically") {
		t.Errorf("the expired note does not say it recovers on its own: %q", expired)
	}
	if strings.Contains(expired, "log in") {
		t.Errorf("the expired note sends the user to re-authenticate: %q", expired)
	}

	dead := SentinelNote("re-login needed")
	if !strings.Contains(dead, "log in") {
		t.Errorf("the dead-lineage note does not say what to do: %q", dead)
	}

	// An unrecognized sentinel still says something.
	if got := SentinelNote("something new"); got != "something new" {
		t.Errorf("SentinelNote = %q", got)
	}
}

func TestAge(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{90 * time.Second, "1m ago"},
		{45 * time.Minute, "45m ago"},
		{3 * time.Hour, "3h ago"},
		{50 * time.Hour, "2d ago"},
	}
	for _, tt := range tests {
		if got := Age(tt.in); got != tt.want {
			t.Errorf("Age(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMoneyGrouping(t *testing.T) {
	tests := []struct {
		amount   float64
		currency string
		want     string
	}{
		{0, "USD", "$0.00"},
		{9.5, "USD", "$9.50"},
		{1000, "USD", "$1,000.00"},
		{1234567.89, "USD", "$1,234,567.89"},
		{-50, "USD", "$-50.00"},
		{100, "EUR", "EUR 100.00"},
	}
	for _, tt := range tests {
		if got := money(tt.amount, tt.currency); got != tt.want {
			t.Errorf("money(%v, %q) = %q, want %q", tt.amount, tt.currency, got, tt.want)
		}
	}
}
