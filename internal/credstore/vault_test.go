package credstore

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/keychain"
	"github.com/d0lim/aaswap/internal/paths"
	"github.com/d0lim/aaswap/internal/platform"
)

// newVaultStore builds a PROVIDER-SCOPED store, which is the only kind that
// uses the vault. An unscoped store is the pre-provider layout by definition —
// see Store.legacy — so a test written against newTestStore would exercise the
// flat shape and pass while proving nothing.
func newVaultStore(t *testing.T) *Store {
	t.Helper()
	r := paths.New(t.TempDir(), platform.Linux)
	return NewForProvider(r, t.TempDir(), keychain.NewWithRunner(newFakeKeychain(), 0),
		"codex", Layout{SecretName: "auth.json", LivePath: r.CodexAuthPath()})
}

// An account's stored files live in a directory of their own.
//
// The point is not tidiness. A flat name encodes "one account is one file", and
// a provider whose login is several files then has nowhere to put the rest —
// which is how a swap comes to copy some of an account and report success.
func TestAnAccountsFilesLiveInItsOwnDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not meaningful on Windows")
	}
	s := newVaultStore(t)
	if err := s.WriteAccount("work", "work@example.com", "creds-work"); err != nil {
		t.Fatal(err)
	}

	dir := s.AccountDir("work", "work@example.com")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("no directory for the account: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
	// Owner-only: it holds a refresh token.
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("permissions = %o, want 700", perm)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("the account's directory is empty")
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s permissions = %o, want 600", entry.Name(), perm)
		}
	}
}

// Two accounts must not share a directory, whatever they are called.
func TestAccountsDoNotShareADirectory(t *testing.T) {
	s := newVaultStore(t)
	for _, account := range []struct{ name, email, value string }{
		{"work", "work@example.com", "creds-work"},
		{"personal", "personal@example.com", "creds-personal"},
		// The same name under a different address, and the reverse: both are
		// distinct accounts and both have to stay distinct on disk.
		{"work", "other@example.com", "creds-other"},
	} {
		if err := s.WriteAccount(account.name, account.email, account.value); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]string{}
	for _, account := range []struct{ name, email, value string }{
		{"work", "work@example.com", "creds-work"},
		{"personal", "personal@example.com", "creds-personal"},
		{"work", "other@example.com", "creds-other"},
	} {
		dir := s.AccountDir(account.name, account.email)
		if previous, clash := seen[dir]; clash {
			t.Fatalf("%s/%s shares a directory with %s", account.name, account.email, previous)
		}
		seen[dir] = account.name + "/" + account.email

		value, unreadable := s.ReadAccount(account.name, account.email)
		if unreadable {
			t.Fatalf("%s became unreadable", account.name)
		}
		if value != account.value {
			t.Errorf("%s reads %q, want %q", account.name, value, account.value)
		}
	}
}

// Deleting an account takes its directory with it. A directory left behind
// keeps a credential aaswap no longer names.
func TestDeletingAnAccountRemovesItsDirectory(t *testing.T) {
	s := newVaultStore(t)
	if err := s.WriteAccount("work", "work@example.com", "creds-work"); err != nil {
		t.Fatal(err)
	}
	dir := s.AccountDir("work", "work@example.com")

	if err := s.DeleteAccount("work", "work@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil {
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("the account's directory survived deletion holding %v", names)
	}
}

// The vault sits beside the working files rather than inside them: the stash
// manifest and the consume locks are not account files and must not land in an
// account's directory.
func TestTheVaultIsSeparateFromTheWorkingFiles(t *testing.T) {
	s := newVaultStore(t)
	if err := s.WriteAccount("work", "work@example.com", "creds-work"); err != nil {
		t.Fatal(err)
	}
	dir := s.AccountDir("work", "work@example.com")
	if strings.HasPrefix(dir, s.CredentialsDir()+string(filepath.Separator)) {
		t.Errorf("the account directory %q is inside the working directory %q",
			dir, s.CredentialsDir())
	}
}

// Every account directory is under one root, so the migration and any future
// audit have a single place to walk.
func TestAccountDirectoriesShareARoot(t *testing.T) {
	s := newVaultStore(t)
	for _, name := range []string{"work", "personal"} {
		if err := s.WriteAccount(name, name+"@example.com", "creds"); err != nil {
			t.Fatal(err)
		}
	}
	root := s.VaultDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading the vault root: %v", err)
	}
	if len(entries) != 2 {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("the vault root holds %v, want one directory per account", names)
	}
}
