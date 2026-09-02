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
// env, defaultDir and secret come from the provider's declaration, so this
// resolves a provider whose name this package has never heard of. An empty
// secret falls back to Claude's layout rather than returning empty: an empty
// path reads the current directory, which is a far worse failure than reading
// the wrong tool's file.
func (r *Resolver) ProviderCredentialsPath(env, defaultDir, secret string) string {
	if secret == "" {
		return r.CredentialsPath()
	}
	return filepath.Join(r.ProviderHome(env, defaultDir), secret)
}
