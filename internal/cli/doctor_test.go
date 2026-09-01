package cli

import (
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/provider"
)

// The report has to name every provider this build offers. One left out is a
// provider whose gaps nobody can see.
func TestDoctorReportsEveryProvider(t *testing.T) {
	h := newHarness(t)
	if code := h.run("doctor"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	for _, name := range provider.Names() {
		if !strings.Contains(h.stdout(), name) {
			t.Errorf("the report does not mention %q:\n%s", name, h.stdout())
		}
	}
}

// The whole point: what a provider cannot do is stated, with a reason.
func TestDoctorExplainsWhatAProviderCannotDo(t *testing.T) {
	h := newHarness(t)
	if code := h.run("doctor"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	out := h.stdout()

	// Codex has no token refresher, and a person needs to know that a stale
	// Codex account means logging in again rather than waiting.
	if !strings.Contains(out, "refresh") {
		t.Errorf("the report never mentions token refresh:\n%s", out)
	}
	// A reason, not just a cross. "codex: refresh tokens ✗" tells nobody what
	// to do about it.
	if !strings.Contains(out, "log in again") {
		t.Errorf("an unsupported capability is reported without a way forward:\n%s", out)
	}
}

// Machines read this too — a wrapper deciding whether to offer `run` for a
// provider should not be parsing prose.
func TestDoctorJSONCarriesTheCapabilityMatrix(t *testing.T) {
	h := newHarness(t)
	if code := h.run("doctor", "--json"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	payload := h.decodeJSON()

	providers, ok := payload["providers"].([]any)
	if !ok || len(providers) == 0 {
		t.Fatalf("payload carries no providers: %v", payload)
	}

	seen := map[string]map[string]any{}
	for _, entry := range providers {
		row, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a provider entry is not an object: %v", entry)
		}
		name, _ := row["name"].(string)
		seen[name] = row
	}

	for _, name := range provider.Names() {
		row, present := seen[name]
		if !present {
			t.Errorf("%q is missing from the JSON report", name)
			continue
		}
		capabilities, ok := row["capabilities"].(map[string]any)
		if !ok {
			t.Errorf("%q carries no capability map: %v", name, row)
			continue
		}
		// Every capability is present for every provider, supported or not.
		// A missing key and an unsupported one are different facts, and a
		// consumer that cannot tell them apart will guess wrong.
		for _, capability := range allCapabilities {
			if _, present := capabilities[string(capability)]; !present {
				t.Errorf("%q does not report %q at all", name, capability)
			}
		}
	}

	// The two that must differ, or the report is not saying anything.
	claude := seen[provider.Claude]["capabilities"].(map[string]any)
	codex := seen[provider.Codex]["capabilities"].(map[string]any)
	if supported(t, claude[string(provider.CapRefresh)]) ==
		supported(t, codex[string(provider.CapRefresh)]) {
		t.Error("claude and codex report the same refresh support; one of them is wrong")
	}
	if !supported(t, codex[string(provider.CapSession)]) {
		t.Error("codex reports no session support, but it declares CODEX_HOME")
	}
}

// A capability aaswap cannot deliver reports a reason a person can act on.
func TestDoctorJSONExplainsEveryGap(t *testing.T) {
	h := newHarness(t)
	if code := h.run("doctor", "--json"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	for _, entry := range h.decodeJSON()["providers"].([]any) {
		row := entry.(map[string]any)
		name := row["name"].(string)
		for capability, raw := range row["capabilities"].(map[string]any) {
			detail, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("%s/%s is not an object: %v", name, capability, raw)
			}
			if detail["supported"] == true {
				continue
			}
			if reason, _ := detail["reason"].(string); strings.TrimSpace(reason) == "" {
				t.Errorf("%s cannot %s and gives no reason", name, capability)
			}
		}
	}
}

// Session support without liveness detection is a real state with real
// consequences, and it has to be visible: it is why a Codex profile is never
// refreshed on its own.
func TestDoctorSaysWhenSessionsCannotBeDetected(t *testing.T) {
	h := newHarness(t)
	if code := h.run("doctor", "--json"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	for _, entry := range h.decodeJSON()["providers"].([]any) {
		row := entry.(map[string]any)
		if row["name"] != provider.Codex {
			continue
		}
		if row["detectsRunningSessions"] != false {
			t.Errorf("codex claims it can detect running sessions: %v", row)
		}
		return
	}
	t.Fatal("codex was not in the report")
}

func supported(t *testing.T, raw any) bool {
	t.Helper()
	detail, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("capability entry is not an object: %v", raw)
	}
	value, _ := detail["supported"].(bool)
	return value
}
