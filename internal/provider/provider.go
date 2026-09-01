// Package provider is the seam between aaswap and the tools whose logins it
// swaps.
//
// # Why a seam at all
//
// Everything above this package is about accounts: which exist, which is live,
// how much quota each has left. None of that differs between Claude Code and
// any other agent CLI. What differs is where a tool keeps its credential, how
// it names the account inside one, and what it does with an isolated profile
// directory — and those differences were spread through paths, credstore,
// session and procdetect as facts about Claude.
//
// # The rule that makes it flexible
//
// An interface here is expressed in terms of what aaswap must DECIDE, never
// where the bytes are. "This read may be a superseded generation" is a decision
// input; "the Keychain was unreadable" is an implementation fact about one
// provider on one operating system. Leak the second and every caller grows a
// macOS branch it has no business having — which is exactly the state this
// package exists to end.
package provider

// ProfileStore is how a provider's tool keeps a credential inside an isolated
// profile directory.
//
// Session mode gives an account its own config directory and lets the tool run
// against it. What the tool then stores THERE is the tool's business, and it
// differs: Claude Code writes a plaintext file and, on macOS, a Keychain item
// keyed by the directory. A file-only tool has just the file.
type ProfileStore interface {
	// Read returns the profile's current credential, or empty when there is
	// none readable.
	//
	// Once a session has run, this — not the backup — is the newest generation
	// of the account's token family. Read-only by contract: rotating what is
	// here would log the next launch out the same way.
	Read(dir string) string

	// MayHold reports whether the profile's credential is anything but
	// definitely absent.
	//
	// An EXISTENCE test, not a read, and false only when every store the
	// provider could use is POSITIVELY empty. Anything indeterminate leans
	// present: the profile may hold the freshest generation of the account's
	// token family, and re-seeding over it would begin by destroying that.
	MayHold(dir string) bool

	// Clear removes anything that would shadow a freshly seeded credential.
	//
	// Best effort, and a no-op for a provider with nothing to shadow WITH.
	// Needed before seeding, and on removal — a name derived from the profile
	// directory cannot be recomputed once the directory is gone.
	Clear(dir string)
}
