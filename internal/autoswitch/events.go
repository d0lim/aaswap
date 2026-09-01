package autoswitch

import (
	json "encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/d0lim/aaswap/internal/jsonout"
)

// Event is one thing the engine did or decided.
//
// The JSONL stream is ADDITIVE: a consumer must ignore event kinds and fields
// it does not recognize, so new kinds can be added without a schema bump.
// Everything a consumer needs is in the payload — an engine watched through a
// log tail should never have to be inferred from prose.
type Event struct {
	SchemaVersion int    `json:"schemaVersion"`
	Kind          string `json:"event"`
	Timestamp     string `json:"ts"`

	// A poll's findings.
	Active *jsonout.AccountRef `json:"active,omitzero"`
	// HeadroomPct is per account, with a null for one whose usage is unknown.
	// Unknown is NOT zero: a row that could not be read is no evidence of an
	// empty account.
	HeadroomPct map[string]*float64 `json:"headroomPct,omitzero"`
	Threshold   *float64            `json:"threshold,omitzero"`
	// FetchErrors names why each unknown account is unknown.
	FetchErrors map[string]string `json:"fetchErrors,omitzero"`
	// WindowsPct is per account, per window. The binding percentage alone hides
	// WHICH window binds, and that ambiguity is what makes a "89%" line
	// unactionable.
	WindowsPct map[string]map[string]float64 `json:"windowsPct,omitzero"`

	// A switch.
	Trigger string              `json:"trigger,omitzero"`
	From    *jsonout.AccountRef `json:"from,omitzero"`
	To      *jsonout.AccountRef `json:"to,omitzero"`
	DryRun  bool                `json:"dryRun,omitzero"`

	// A decision not to switch, a quarantine, or a failure.
	Reason  string `json:"reason,omitzero"`
	Detail  string `json:"detail,omitzero"`
	Name    string `json:"number,omitzero"`
	Email   string `json:"email,omitzero"`
	Message string `json:"message,omitzero"`
	// Transient marks a failure the engine will retry. False means it will keep
	// happening until something outside the engine changes.
	Transient *bool `json:"transient,omitzero"`

	// A wait.
	Seconds *float64 `json:"seconds,omitzero"`
	Until   string   `json:"until,omitzero"`
	// EarliestResetAt is when the fleet's soonest quota returns, or empty when
	// nothing said.
	EarliestResetAt string `json:"earliestResetAt,omitzero"`
}

// Event kinds.
const (
	KindPoll          = "poll"
	KindSwitch        = "switch"
	KindNoSwitch      = "no-switch"
	KindQuarantine    = "quarantine"
	KindUnquarantine  = "unquarantine"
	KindAllExhausted  = "all-exhausted"
	KindSleep         = "sleep"
	KindError         = "error"
	KindConfigWarning = "config-warning"
)

// Human renders an event as one line a person can read while watching a log.
func (e Event) Human() string {
	switch e.Kind {
	case KindPoll:
		return e.humanPoll()
	case KindSwitch:
		prefix := "switched"
		if e.DryRun {
			prefix = "would switch"
		}
		return fmt.Sprintf("%s %s → %s (%s)", prefix, refLabel(e.From), refLabel(e.To), e.Trigger)
	case KindNoSwitch:
		if e.Detail != "" {
			return fmt.Sprintf("no switch: %s (%s)", e.Reason, e.Detail)
		}
		return "no switch: " + e.Reason
	case KindQuarantine:
		return fmt.Sprintf("account %s (%s) quarantined: %s. Log in with it and run "+
			"`aaswap add --slot %s` to bring it back", e.Name, e.Email, e.Reason, e.Name)
	case KindUnquarantine:
		return fmt.Sprintf("account %s (%s) is back in rotation (%s)", e.Name, e.Email, e.Reason)
	case KindAllExhausted:
		if e.EarliestResetAt != "" {
			return "every account is exhausted; the earliest reset is " + e.EarliestResetAt
		}
		return "every account is exhausted, and none reported a reset time"
	case KindSleep:
		if e.Seconds != nil {
			return fmt.Sprintf("sleeping %.0fm (until %s)", *e.Seconds/60, e.Until)
		}
		return "sleeping until " + e.Until
	case KindError:
		if e.Transient != nil && *e.Transient {
			return "error: " + e.Message + " (will retry)"
		}
		return "error: " + e.Message
	case KindConfigWarning:
		return "warning: " + e.Message
	}
	return e.Kind
}

func (e Event) humanPoll() string {
	if e.Active == nil {
		return "poll: no active account"
	}
	activeNum := e.Active.Name

	var used string
	switch headroom, known := e.HeadroomPct[activeNum]; {
	case known && headroom != nil:
		used = fmt.Sprintf("%.0f%% used", 100-*headroom)
		// The binding percentage alone hides WHICH window binds, and for the
		// account the decision is about that is exactly where it matters.
		if windows := e.WindowsPct[activeNum]; len(windows) > 1 {
			used += " [" + e.describe(activeNum) + "]"
		}
	default:
		if reason, named := e.FetchErrors[activeNum]; named {
			used = "usage unknown (" + reason + ")"
		} else {
			used = "usage unknown"
		}
	}

	threshold := ""
	if e.Threshold != nil {
		threshold = fmt.Sprintf(" (switch at %s%%)", pctLabel(*e.Threshold))
	}

	var others []string
	for _, num := range slices.Sorted(maps.Keys(e.HeadroomPct)) {
		if num == activeNum {
			continue
		}
		others = append(others, "#"+num+": "+e.describe(num))
	}
	tail := ""
	if len(others) > 0 {
		tail = " | others: " + strings.Join(others, ", ")
	}
	return fmt.Sprintf("account %s (%s): %s%s%s", activeNum, e.Active.Email, used, threshold, tail)
}

// describe summarizes one account for a poll line.
//
// The per-window breakdown when there is one, because the binding percentage
// alone hides which window binds.
func (e Event) describe(num string) string {
	if windows := e.WindowsPct[num]; len(windows) > 0 {
		var parts []string
		for _, label := range windowOrder(windows) {
			parts = append(parts, fmt.Sprintf("%s %.0f%%", label, windows[label]))
		}
		return strings.Join(parts, " · ")
	}
	if headroom, known := e.HeadroomPct[num]; known && headroom != nil {
		return fmt.Sprintf("%.0f%%", 100-*headroom)
	}
	if reason, named := e.FetchErrors[num]; named {
		return "? (" + reason + ")"
	}
	return "?"
}

// windowOrder puts the account-wide windows first, then the per-model ones
// alphabetically — so a line reads the same way every time.
func windowOrder(windows map[string]float64) []string {
	var out []string
	for _, label := range []string{"5h", "7d"} {
		if _, present := windows[label]; present {
			out = append(out, label)
		}
	}
	var scoped []string
	for label := range windows {
		if label != "5h" && label != "7d" {
			scoped = append(scoped, label)
		}
	}
	slices.Sort(scoped)
	return append(out, scoped...)
}

func refLabel(ref *jsonout.AccountRef) string {
	if ref == nil {
		return "(none)"
	}
	if ref.Name == "" {
		return ref.Email
	}
	return fmt.Sprintf("%s (%s)", ref.Name, ref.Email)
}

// pctLabel renders a percentage without claiming precision it does not have.
//
// One decimal only when there is one, because both sides of a comparison go
// through here: rounding to whole numbers could display an impossible
// "100% < 99.9%".
func pctLabel(value float64) string {
	if value == float64(int64(value)) {
		return fmt.Sprintf("%.0f", value)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", value), "0"), ".")
}

// Emitter receives events.
//
// An interface so the CLI can render them as prose or as JSONL without the
// engine knowing which, and so a test can collect them.
type Emitter interface {
	Emit(Event)
}

// EmitterFunc adapts a function to an Emitter.
type EmitterFunc func(Event)

// Emit calls the function.
func (f EmitterFunc) Emit(event Event) { f(event) }

// JSONLine renders an event as one line of the JSONL stream.
func (e Event) JSONLine() ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// newEvent stamps an event with the contract's version and the time.
func newEvent(kind string, now time.Time) Event {
	return Event{
		SchemaVersion: jsonout.SchemaVersion,
		Kind:          kind,
		Timestamp:     jsonout.Timestamp(now),
	}
}
