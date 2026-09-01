package paths

import "path/filepath"

// CodexHomeEnv relocates Codex's whole directory, the way CLAUDE_CONFIG_DIR
// relocates Claude Code's.
const CodexHomeEnv = "CODEX_HOME"

// CodexHome is where the Codex CLI keeps everything.
func (r *Resolver) CodexHome() string {
	if r.CodexHomeDir != "" {
		return r.CodexHomeDir
	}
	return filepath.Join(r.Home, ".codex")
}

// CodexAuthPath is Codex's credential file.
//
// One file where Claude Code has two: it holds the auth mode, the API key when
// there is one, and the OAuth token set — and the id_token inside that token
// set IS the identity document. There is no separate config to read an address
// out of, which is why the provider seam has to carry identity extraction
// rather than assuming a config field.
func (r *Resolver) CodexAuthPath() string {
	return filepath.Join(r.CodexHome(), "auth.json")
}

// ProviderCredentialsPath is where a provider's tool keeps its live credential.
//
// An unrecognized provider falls back to the default rather than returning
// empty: an empty path is a read of the current directory, which is a far worse
// failure than reading the wrong tool's file.
func (r *Resolver) ProviderCredentialsPath(provider string) string {
	if provider == "codex" {
		return r.CodexAuthPath()
	}
	return r.CredentialsPath()
}
