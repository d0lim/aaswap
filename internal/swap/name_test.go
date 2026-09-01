package swap

import "testing"

// A name is now the account's handle AND a path component: it names the
// credential file and the Keychain item. Everything the old alias rules allowed
// but a filename must not carry has to be refused here rather than discovered
// as a stray file somewhere.
func TestNormalizeName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"lowercases and trims", "  Work  ", "work", false},
		{"keeps the allowed punctuation", "team_a.dev-2", "team_a.dev-2", false},
		{"empty is not a name", "", "", true},
		{"whitespace alone is not a name", "   ", "", true},
		// Purely numeric was reserved for slot numbers, and slot numbers are
		// gone — but a bare number is still a terrible handle, because it reads
		// as the thing it replaced in every shell history on the machine.
		{"purely numeric is refused", "2", "", true},
		{"a leading dash reads as a flag", "-x", "", true},
		{"a separator cannot appear", "a/b", "", true},
		{"a backslash cannot appear either", `a\b`, "", true},
		// The three that the alias rules let through and a path component must
		// not. Harmless as a label, a traversal as a filename.
		{"a single dot is not a name", ".", "", true},
		{"a double dot is not a name", "..", "", true},
		{"any all-dots run is not a name", "...", "", true},
		// A leading dot is fine — it is a hidden file at worst, not an escape.
		{"a leading dot is allowed", ".ssh", ".ssh", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeName(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("NormalizeName(%q) = %q, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeName(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// A default name comes from the address, because that is the only thing every
// account has. It has to survive addresses that carry nothing usable.
func TestNameForEmail(t *testing.T) {
	tests := []struct {
		email string
		want  string
	}{
		{"work@example.com", "work"},
		{"Work.Two@Example.com", "work.two"},
		{"setup-token-1@token.local", "setup-token-1"},
		// Plus-addressing names one inbox, not one account.
		{"work+claude@example.com", "work"},
		// Nothing usable in front of the @, and nothing usable at all: both
		// still have to produce a name, or the account cannot be addressed.
		{"@example.com", "account"},
		{"", "account"},
		{"...@example.com", "account"},
		{"2@example.com", "account-2"},
	}
	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			got := NameForEmail(tt.email)
			if got != tt.want {
				t.Errorf("NameForEmail(%q) = %q, want %q", tt.email, got, tt.want)
			}
			// Whatever it produces must itself be a legal name, or the default
			// path hands the store something the guard would have refused.
			if _, err := NormalizeName(got); err != nil {
				t.Errorf("NameForEmail(%q) = %q, which is not a legal name: %v", tt.email, got, err)
			}
		})
	}
}

// The same address legitimately exists across a personal account and one or
// more organizations. Those are different accounts and need different names.
func TestUniqueNameSuffixesACollision(t *testing.T) {
	taken := map[string]bool{}
	var got []string
	for range 3 {
		name := uniqueName("work", taken)
		taken[name] = true
		got = append(got, name)
	}
	want := []string{"work", "work-2", "work-3"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("names = %v, want %v", got, want)
			break
		}
	}
}
