// Package testutil holds helpers shared across aaswap's test suites.
//
// It exists for one job so far: making a file genuinely unreadable, which
// several packages need and which no two operating systems agree on.
//
// The distinction between "unreadable" and "absent" is load-bearing in this
// codebase — a credential that exists but cannot be read must never be treated
// as one that was never there, or a slot gets re-added, a relocation commits
// against an empty destination, or a strike is cleared and a dead account
// leaves quarantine. Every test of that rule needs a real read failure, not a
// simulated one, so the helper is per-platform rather than a fake.
package testutil

import "testing"

// MakeUnreadable makes path fail to read for the rest of the test, restoring it
// afterwards.
//
// It skips the test when the platform cannot produce the condition — running as
// root, where permission bits deny nothing, or asking for an unreadable
// DIRECTORY on Windows, where that needs an ACL rewrite rather than a mode.
func MakeUnreadable(t *testing.T, path string) {
	t.Helper()
	makeUnreadable(t, path)
}
