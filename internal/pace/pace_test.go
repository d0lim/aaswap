package pace

import (
	"math"
	"testing"
	"time"
)

// fetchedAt is an arbitrary but fixed reference point. Every window below is
// positioned relative to it, so the tests read as "N days into the week".
var fetchedAt = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// weekly builds a window that is `into` through its weekly cycle at fetchedAt.
func weekly(pct float64, into time.Duration) Window {
	return Window{Pct: pct, ResetsAt: fetchedAt.Add(WeeklyPeriod - into), Valid: true}
}

func TestComputeElapsedAndExpected(t *testing.T) {
	tests := []struct {
		name         string
		window       Window
		wantOK       bool
		wantElapsed  time.Duration
		wantExpected float64
	}{
		{
			name:         "one day into the week",
			window:       weekly(30, 24*time.Hour),
			wantOK:       true,
			wantElapsed:  24 * time.Hour,
			wantExpected: 100.0 / 7,
		},
		{
			name:         "halfway through the week",
			window:       weekly(50, 84*time.Hour),
			wantOK:       true,
			wantElapsed:  84 * time.Hour,
			wantExpected: 50,
		},
		{
			// A reset timestamp several cycles in the past — a stale value that
			// was never rolled forward — still resolves to the current cycle.
			name:         "a reset three cycles in the past still resolves",
			window:       Window{Pct: 30, ResetsAt: fetchedAt.Add(-3*WeeklyPeriod - 24*time.Hour), Valid: true},
			wantOK:       true,
			wantElapsed:  24 * time.Hour,
			wantExpected: 100.0 / 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Compute(tt.window, fetchedAt, Options{})
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got.Elapsed != tt.wantElapsed {
				t.Errorf("Elapsed = %v, want %v", got.Elapsed, tt.wantElapsed)
			}
			if math.Abs(got.ExpectedPct-tt.wantExpected) > 1e-9 {
				t.Errorf("ExpectedPct = %v, want %v", got.ExpectedPct, tt.wantExpected)
			}
		})
	}
}

// Right after a reset the expected percentage is near zero, so almost any usage
// would read as far ahead. That is a false positive, not a warning.
func TestComputeSuppressesJustAfterAReset(t *testing.T) {
	tests := []struct {
		name   string
		into   time.Duration
		wantOK bool
	}{
		{"exactly at the reset boundary", 0, false},
		{"an hour in", time.Hour, false},
		{"just inside the suppression window", SuppressAfterReset - time.Minute, false},
		{"exactly at the suppression edge", SuppressAfterReset, true},
		{"just outside it", SuppressAfterReset + time.Minute, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := Compute(weekly(90, tt.into), fetchedAt, Options{})
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
		})
	}
}

func TestComputeRejectsUnusableInput(t *testing.T) {
	tests := []struct {
		name      string
		window    Window
		fetchedAt time.Time
	}{
		{"an invalid window", Window{Pct: 50, ResetsAt: fetchedAt, Valid: false}, fetchedAt},
		{"no reset timestamp", Window{Pct: 50, Valid: true}, fetchedAt},
		{"no fetch time", weekly(50, 48*time.Hour), time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := Compute(tt.window, tt.fetchedAt, Options{}); ok {
				t.Error("Compute reported a usable result for unusable input")
			}
		})
	}
}

// The threshold is what keeps the marker off already-dense usage rows for
// variance that means nothing.
func TestComputeAheadThreshold(t *testing.T) {
	// Two days in: expected is 2/7 ≈ 28.57%.
	const twoDaysExpected = 200.0 / 7

	tests := []struct {
		name      string
		pct       float64
		wantAhead bool
	}{
		{"meaningfully ahead", twoDaysExpected + AheadThresholdPct + 5, true},
		{"exactly at the threshold", twoDaysExpected + AheadThresholdPct, true},
		{"just inside the threshold", twoDaysExpected + AheadThresholdPct - 1, false},
		{"on pace", twoDaysExpected, false},
		{"behind pace", twoDaysExpected - 10, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Compute(weekly(tt.pct, 48*time.Hour), fetchedAt, Options{})
			if !ok {
				t.Fatal("Compute reported no result")
			}
			if got.Ahead != tt.wantAhead {
				t.Errorf("Ahead = %v, want %v (actual %.2f vs expected %.2f)",
					got.Ahead, tt.wantAhead, got.ActualPct, got.ExpectedPct)
			}
		})
	}
}

// The marker cannot fire in the last day or so of the week, because expected
// has passed (100 - threshold) and the percentage tops out at 100. By then the
// percentage itself tells the story.
func TestComputeCannotFlagAheadLateInTheWeek(t *testing.T) {
	got, ok := Compute(weekly(100, WeeklyPeriod-2*time.Hour), fetchedAt, Options{})
	if !ok {
		t.Fatal("Compute reported no result")
	}
	if got.Ahead {
		t.Errorf("Ahead = true at %.2f%% expected; the marker should be unreachable this late",
			got.ExpectedPct)
	}
}

func TestProjectedExhaustion(t *testing.T) {
	// A window spent exactly on pace exhausts exactly at its own reset: 25%
	// used a quarter of the way through leaves 75% to burn over the remaining
	// three quarters. The projection runs from the FETCH time, not from the
	// window's start, which is what makes the reset the right landing point.
	t.Run("a constant on-pace rate lands at the reset", func(t *testing.T) {
		w := weekly(25, WeeklyPeriod/4)
		r, ok := Compute(w, fetchedAt, Options{})
		if !ok {
			t.Fatal("Compute reported no result")
		}
		got, ok := ProjectedExhaustion(r, fetchedAt)
		if !ok {
			t.Fatal("ProjectedExhaustion reported no projection")
		}
		if diff := got.Sub(w.ResetsAt); diff > time.Minute || diff < -time.Minute {
			t.Errorf("projection = %v, want the reset at %v", got, w.ResetsAt)
		}
	})

	// Double the on-pace rate exhausts halfway through the remaining time.
	t.Run("a faster rate lands earlier", func(t *testing.T) {
		w := weekly(50, WeeklyPeriod/4)
		r, ok := Compute(w, fetchedAt, Options{})
		if !ok {
			t.Fatal("Compute reported no result")
		}
		got, ok := ProjectedExhaustion(r, fetchedAt)
		if !ok {
			t.Fatal("ProjectedExhaustion reported no projection")
		}
		if !got.Before(w.ResetsAt) {
			t.Errorf("projection = %v, want it before the reset at %v", got, w.ResetsAt)
		}
	})

	t.Run("already at or over 100 projects to now", func(t *testing.T) {
		r, ok := Compute(weekly(100, 48*time.Hour), fetchedAt, Options{})
		if !ok {
			t.Fatal("Compute reported no result")
		}
		got, ok := ProjectedExhaustion(r, fetchedAt)
		if !ok || !got.Equal(fetchedAt) {
			t.Errorf("projection = (%v, %v), want the fetch time", got, ok)
		}
	})

	t.Run("zero usage has no projection", func(t *testing.T) {
		r, ok := Compute(weekly(0, 48*time.Hour), fetchedAt, Options{})
		if !ok {
			t.Fatal("Compute reported no result")
		}
		if _, ok := ProjectedExhaustion(r, fetchedAt); ok {
			t.Error("ProjectedExhaustion reported a projection with no measurable rate")
		}
	})
}

func TestWillLastToReset(t *testing.T) {
	tests := []struct {
		name     string
		pct      float64
		into     time.Duration
		wantOK   bool
		wantLast bool
	}{
		// Half the budget at half the week projects to exactly 100.
		{"exactly on pace lasts", 50, WeeklyPeriod / 2, true, true},
		{"a comfortably sustainable rate lasts", 20, WeeklyPeriod / 2, true, true},
		{"an unsustainable rate does not", 80, WeeklyPeriod / 2, true, false},
		{"zero usage lasts", 0, 48 * time.Hour, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, ok := Compute(weekly(tt.pct, tt.into), fetchedAt, Options{})
			if !ok {
				t.Fatal("Compute reported no result")
			}
			got, ok := WillLastToReset(r)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.wantLast {
				t.Errorf("WillLastToReset = %v, want %v", got, tt.wantLast)
			}
		})
	}
}

// At a constant rate the projected total is 100 × actual/expected, so this
// flips exactly when the window is over expected at all — the marker's signal
// with no threshold. Scripts get the sensitive answer, the UI the noise-gated
// one.
func TestWillLastFlipsAtExpectedWhileTheMarkerStaysQuiet(t *testing.T) {
	// Two days in, a few points over expected: under the marker's threshold,
	// but already projecting past 100.
	const twoDaysExpected = 200.0 / 7
	r, ok := Compute(weekly(twoDaysExpected+2, 48*time.Hour), fetchedAt, Options{})
	if !ok {
		t.Fatal("Compute reported no result")
	}

	if r.Ahead {
		t.Error("Ahead = true for a window only slightly over expected")
	}
	lasts, ok := WillLastToReset(r)
	if !ok {
		t.Fatal("WillLastToReset reported no answer")
	}
	if lasts {
		t.Error("WillLastToReset = true for a window already over expected")
	}
}

// The 5-hour window is excluded from pace entirely, but the period is a
// parameter, so a caller that passes a short one still gets coherent answers.
func TestOptionsOverrideTheDefaults(t *testing.T) {
	opts := Options{
		Period:             24 * time.Hour,
		SuppressAfterReset: time.Hour,
		AheadThresholdPct:  1,
	}
	w := Window{Pct: 60, ResetsAt: fetchedAt.Add(12 * time.Hour), Valid: true}

	got, ok := Compute(w, fetchedAt, opts)
	if !ok {
		t.Fatal("Compute reported no result")
	}
	if got.Period != 24*time.Hour {
		t.Errorf("Period = %v, want the override", got.Period)
	}
	if math.Abs(got.ExpectedPct-50) > 1e-9 {
		t.Errorf("ExpectedPct = %v, want 50 at the halfway point of a one-day period", got.ExpectedPct)
	}
	if !got.Ahead {
		t.Error("Ahead = false at 60%% against 50%% expected with a 1-point threshold")
	}
}

// The defaults are a UX contract: a week, a day of post-reset quiet, and a
// 15-point gap before the marker shows.
func TestDefaults(t *testing.T) {
	got := Options{}.withDefaults()
	if got.Period != WeeklyPeriod {
		t.Errorf("Period = %v, want %v", got.Period, WeeklyPeriod)
	}
	if got.SuppressAfterReset != SuppressAfterReset {
		t.Errorf("SuppressAfterReset = %v, want %v", got.SuppressAfterReset, SuppressAfterReset)
	}
	if got.AheadThresholdPct != AheadThresholdPct {
		t.Errorf("AheadThresholdPct = %v, want %v", got.AheadThresholdPct, AheadThresholdPct)
	}
}
