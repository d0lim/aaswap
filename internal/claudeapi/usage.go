package claudeapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/d0lim/aaswap/internal/usage"
)

// rawWindow is one legacy account-wide window as the endpoint reports it.
//
// Utilization is a pointer because a window object that omits it is malformed
// and must be skipped, not read as zero — a missing percentage rendered as 0%
// would tell the user they have a full budget they may not have.
type rawWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// rawExtra is the pay-as-you-go extra-usage block. All three numbers are
// nullable, and a nil MonthlyLimit specifically means unlimited.
type rawExtra struct {
	IsEnabled    bool     `json:"is_enabled"`
	UsedCredits  *float64 `json:"used_credits"`
	MonthlyLimit *float64 `json:"monthly_limit"`
	Utilization  *float64 `json:"utilization"`
	Currency     string   `json:"currency"`
	ResetsAt     string   `json:"resets_at"`
}

// rawLimit is one entry of the newer limits array, which is where per-model
// weekly windows live.
type rawLimit struct {
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

// rawUsage is the usage endpoint's body.
type rawUsage struct {
	FiveHour   *rawWindow `json:"five_hour"`
	SevenDay   *rawWindow `json:"seven_day"`
	ExtraUsage *rawExtra  `json:"extra_usage"`
	Limits     []rawLimit `json:"limits"`
}

// creditsPerUnit converts the endpoint's integer credits to currency units.
const creditsPerUnit = 100

// normalize maps a raw usage response to the stored shape, returning nil when
// the response carried no usable data at all.
//
// Each section is independent: a malformed one is dropped and the others go
// through. A partial answer about the windows that do gate this account is
// strictly better than none, and the alternative — failing the whole fetch on
// one bad section — would spend a request from the hourly budget and store
// nothing.
func (r *rawUsage) normalize() *usage.Result {
	var result usage.Result

	result.FiveHour = normalizeWindow(r.FiveHour)
	result.SevenDay = normalizeWindow(r.SevenDay)
	result.Spend = normalizeSpend(r.ExtraUsage)

	// Per-model weekly limits are invisible to the legacy five_hour/seven_day
	// keys above, so each is surfaced separately. An older response with no
	// limits array simply yields none.
	for _, lim := range r.Limits {
		if lim.Percent == nil || lim.Scope == nil || lim.Scope.Model == nil {
			continue
		}
		name := lim.Scope.Model.DisplayName
		if name == "" {
			continue
		}
		result.Scoped = append(result.Scoped, usage.Scoped{
			Name:     name,
			Pct:      *lim.Percent,
			ResetsAt: lim.ResetsAt,
		})
	}

	if result.Empty() {
		return nil
	}
	return &result
}

func normalizeWindow(w *rawWindow) *usage.Window {
	if w == nil || w.Utilization == nil {
		return nil
	}
	return &usage.Window{Pct: *w.Utilization, ResetsAt: w.ResetsAt}
}

func normalizeSpend(e *rawExtra) *usage.Spend {
	// A nil MonthlyLimit means unlimited, which has no percentage to render, so
	// the spend line is dropped while the gating windows go through unchanged.
	if e == nil || !e.IsEnabled || e.UsedCredits == nil || e.MonthlyLimit == nil || e.Utilization == nil {
		return nil
	}
	currency := e.Currency
	if currency == "" {
		currency = "USD"
	}
	return &usage.Spend{
		Used:     *e.UsedCredits / creditsPerUnit,
		Limit:    *e.MonthlyLimit / creditsPerUnit,
		Pct:      *e.Utilization,
		Currency: currency,
		ResetsAt: e.ResetsAt,
	}
}

// FetchUsage requests and normalizes an account's rate-limit usage.
//
// A nil result with a nil error is a successful round trip whose response
// carried no window data — the account has no limits to report, which is not a
// failure.
//
// Must not be called while any credential or config lock is held.
func (c *Client) FetchUsage(ctx context.Context, accessToken string) (*usage.Result, error) {
	req, err := c.newRequest(http.MethodGet, c.UsageURL, nil, map[string]string{
		"Authorization":  "Bearer " + accessToken,
		"anthropic-beta": BetaHeader,
	})
	if err != nil {
		return nil, err
	}
	raw, err := c.do(ctx, req, c.UsageTimeout)
	if err != nil {
		return nil, err
	}
	var body rawUsage
	if err := decodeJSON(raw, &body); err != nil {
		return nil, err
	}
	return body.normalize(), nil
}

// UsageOutcome is the result of a usage fetch attempt for one account.
type UsageOutcome struct {
	// Usage is the normalized measurement on success. It can be nil on a
	// successful round trip whose response carried no window data, so Error is
	// what distinguishes success from failure — not this field.
	Usage *usage.Result

	// Error is empty on success, else the classified kind.
	Error ErrorKind

	// RetryAfter carries the server's Retry-After when it sent one. Nil means
	// the server said nothing; a zero duration means it said "now", and the two
	// are deliberately distinguishable.
	RetryAfter *time.Duration

	// StruckFP fingerprints the credential whose refresh token was POSTed, set
	// only when Error is a permanent auth verdict. It lets the store bind the
	// strike to that generation rather than to whatever the slot holds by the
	// time the strike is recorded.
	StruckFP string
}

// OK reports whether the fetch completed without error.
func (o UsageOutcome) OK() bool { return o.Error == "" }

// logUsageFailure emits one WARNING line naming the cause, so a failing
// endpoint is diagnosable from the default log file rather than only under a
// debug flag. The line is what users paste into public issues, so context must
// never carry an email address.
func logUsageFailure(context string, kind ErrorKind, retry *time.Duration, err error) {
	attrs := []any{"kind", kind}
	if context != "" {
		attrs = append(attrs, "context", context)
	}
	if retry != nil {
		attrs = append(attrs, "retry_after_s", retry.Seconds())
	}
	message := "usage fetch failed"
	if kind == HTTPKind(http.StatusTooManyRequests) {
		// Whether the budget counts per access token or per account depends on
		// the org's rate-limit regime (both are in the wild; see pollpolicy),
		// so the message stays scope-neutral. Under the account-scoped regime
		// re-authenticating does not clear a block, and two machines holding
		// different tokens for one account still compete for one budget.
		message += " (usage-endpoint budget reached; backing off)"
	}
	slog.Warn(message, attrs...)
	slog.Debug("usage fetch failure detail", "context", context, "error", err)
}
