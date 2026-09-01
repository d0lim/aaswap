# aaswap

**A**gent **A**ccount **Swap**. Switch between agent CLI accounts without
logging out. See every account's remaining quota at a glance, and run several
accounts in parallel.

Manages **Claude Code** (the CLI and the VS Code extension) and **Codex**. Each
provider keeps its own accounts and its own active login, so both tools can be
signed in at once. Adding another agent CLI is a declaration rather than a port
— see [Adding a provider](#adding-a-provider).

`aaswap doctor` reports what works for which provider, so nothing is left to
guess at.

A single static binary — no runtime to install, on macOS, Linux or Windows.

## Installation

aaswap is a single static binary. It needs no runtime, no interpreter, and no
package index.

### Homebrew

```bash
brew install --cask d0lim/tap/aaswap
```

A cask rather than a formula because aaswap ships as a prebuilt binary, which is
what casks are for — the distinction is source-versus-prebuilt, not app-versus-CLI.
Linux needs Homebrew 4.5.0 or newer, which is where Linux cask support landed.

### Go

```bash
go install github.com/d0lim/aaswap/cmd/aaswap@latest
```

### Download a binary

Grab the build for your platform from the [releases page](https://github.com/d0lim/aaswap/releases),
unpack it, and put `aaswap` somewhere on your `PATH`.

### From source

```bash
git clone https://github.com/d0lim/aaswap.git
cd aaswap
make build          # produces ./aaswap
```

### Updating

```bash
aaswap upgrade       # reports the latest release and the command for your install
```

`aaswap upgrade` does not replace the binary in place. A binary installed by a
package manager belongs to that package manager, and swapping it out from
underneath would leave its records wrong — so aaswap tells you the command
instead of guessing.

## Usage

### Add your first account

Log into your agent CLI, then:

```bash
aaswap login
```

`login` looks at what is live and says what it will do. With an account logged
in that aaswap does not yet store, that is simply to store it.

### Add more accounts

aaswap cannot log you in — the agent CLI owns that flow — so adding another
account means logging in with it. `login` closes the gap from its side: it
prints what to do and captures the account the moment the login lands.

```bash
aaswap login
```

```
  Logged in as work@example.com [Acme] — already stored as work.

    [r] refresh that account's stored credential
    [w] wait for a different login, then add it
    [q] cancel

  [r/w/q]
```

Answer `w`, run `claude` in another terminal, `/login` with the other account,
and come back to find it added.

Do not run `/logout` first: current Claude Code may revoke the refresh token
stored for the account you are leaving.

Three flags skip the question, for scripts and for people who already know:

```bash
aaswap login --capture    # store the account logged in now
aaswap login --wait       # wait for a /login, then store that account
aaswap login --token -    # read a setup token or API key from stdin
```

Without a terminal nothing is asked and nothing waits: an unstored live login
is captured, a stored one is refreshed, and no login at all is an error.

### Accounts are named, not numbered

Every account has a name — its address's local part by default, suffixed when
two accounts would collide. Use it anywhere:

```bash
aaswap switch work
aaswap account rename work dev
```

### Two providers

`--provider` picks the auth domain, defaulting to `claude`:

```bash
aaswap --provider codex login          # store a Codex account
aaswap --provider codex list
AASWAP_PROVIDER=codex aaswap switch work
```

Each provider has its own accounts and its own active login. Switching one
never touches the other.

Codex quota comes from what Codex itself recorded during your last session, so
it costs no request and consumes nothing — but it can only describe the account
you are signed into now. Idle Codex accounts show no measurement rather than
stale numbers.

`run` and `dir` work for any provider that declares an isolated profile
directory — both Claude Code and Codex do. For Codex, aaswap cannot tell whether
a session is running against a profile, so it never replaces a profile's
credential on its own and says so instead.

Run `aaswap doctor` for the full table.

### Switch accounts

Rotate to the next account:

```bash
aaswap switch
```

Or switch to a specific account:

```bash
aaswap switch 2
aaswap switch user@example.com
aaswap switch dev                # or by whatever you renamed it to
```

Not sure which one? `aaswap list` shows every account's 5-hour and 7-day usage and reset times at a glance:

```bash
aaswap list
```

### The dashboard

`aaswap tui` is the same information as an interactive screen: usage bars per
account, reset times, and switching without retyping a name.

```bash
aaswap tui
```

```
 aaswap                                                     watch on

 ▸ ● 1 work@example.com  (work)                                  Acme
     5h  ██████████████▉░░░░░░░░░  62%   resets 20:39
     7d  ███████▍░░░░░░░░░░░░░░░░  31%   resets Jul 5 22:12

   ○ 2 spare@example.com                                     personal
     5h  ██▋░░░░░░░░░░░░░░░░░░░░░  11%   resets 18:02
     7d  ████▌░░░░░░░░░░░░░░░░░░░  19%   resets Jul 7 12:12

   ↑↓ move  ·  enter switch  ·  a add  ·  t token  ·  q quit  ·  ? help
```

Accounts can be added without leaving the dashboard:

| Key | |
|---|---|
| `a` | add the account you are logged in as (or refresh it, if already stored) |
| `n` | wait for a `/login` elsewhere, then add that account |
| `t` | paste a setup token or managed API key |
| `enter` | switch to the selected account |
| `d` | hold an account out of rotation, or return it |
| `r` / `w` | collect now / re-collect every 30 seconds |
| `?` | every key, including the ones the footer drops on a narrow terminal |

`n` is the dashboard's form of `aaswap login --wait`: it keeps watching while you
run `/login` in another terminal and captures the account when it appears.
Switching asks before it replaces a live credential. The dashboard needs a
terminal — in a script, use `aaswap list --json`.

**Note:** You usually don't need to restart — on Linux/Windows the new account is picked up automatically, and on macOS after the Keychain cache expires. To apply it instantly, restart Claude Code or reopen the VS Code extension tab. See [Tips](#tips) for the per-platform details.

### What aaswap does not do

aaswap has no automatic rotation. There is no daemon polling every account's
quota and moving the live login off one that is running low.

That existed and was removed. Automatically rotating between accounts to keep
working past a rate limit is circumvention however it is dressed up, and it
leaves a signature no ordinary single user produces: several accounts' tokens
polled from one machine on a schedule, and one account stopping at its threshold
while another starts seconds later, repeatedly. The risk of that landed on
whoever ran it, not on whoever wrote it.

Everything else here is managing several accounts one person legitimately has.
`aaswap switch` with no argument still rotates to the next account, when you
decide to.

### Run multiple accounts at the same time (session mode)

Launch Claude Code as a specific account in the current terminal only — every other terminal and the VS Code extension stay on your default account, so two accounts can work in parallel.

```bash
aaswap run 2                     # launch Claude Code as account 2, here only
aaswap run user@example.com      # by email
aaswap run 2 -- --resume         # everything after '--' is forwarded to claude
aaswap run 2 --share-history     # share your chat history with this account too
```

Sessions use your normal `~/.claude` setup (settings, CLAUDE.md, skills, MCP servers, etc.), but each account keeps its own chat history — pass `--share-history` if you want your accounts to continue the same conversations.

<details>
<summary>Sharing details — MCP servers & chat history</summary>

- With `--share-history`, a session started under one account shows up in `--resume` under the others, and nothing already saved is lost.
- User-scope MCP servers (`claude mcp add -s user`) are mirrored from your default profile on every launch — manage them there; changes made inside a session don't persist. Definitions are copied as-is (including inline `env`/`headers` values), but MCP OAuth logins are not — HTTP servers may ask you to authenticate once per profile via `/mcp`.
- `--no-share` turns sharing off and removes the mirrored MCP config (profiles that never mirrored are left alone).

</details>

<details>
<summary>Map accounts to directories — auto-pick per repo</summary>

Bind a directory to an account, and a bare `aaswap run` there launches that account in session mode — e.g. work account in work repos, personal elsewhere:

```bash
aaswap dir map work ~/work/client-app   # map a directory to the account "work"
aaswap dir map user@example.com         # map the current directory
aaswap dir list                         # list mappings
aaswap dir unmap ~/work/client-app      # remove one (defaults to current directory)

cd ~/work/client-app/src
aaswap run                       # → account 2, session mode
```

Subfolders inherit the nearest mapped ancestor. In an unmapped directory, `aaswap run` just launches plain `claude` with your default login. Mappings are per-machine (not part of `aaswap account export`) and are cleaned up when their account is removed.

</details>

### Refresh expired tokens

If an account's token expires, log back into the agent CLI with that account
and run:

```bash
aaswap login
```

aaswap notices the stored credential was already refused and refreshes it in
place without asking — that is the one thing being logged in as it again could
mean.

### Other commands

```bash
aaswap list                      # every account with 5h/7d usage and reset times
aaswap status                    # which account is currently logged in
aaswap switch work               # activate an account
aaswap switch                    # rotate to the next one
aaswap run work                  # run an account in this terminal only (session mode)
aaswap tui                       # interactive dashboard
aaswap doctor                    # what aaswap can do for each provider
aaswap config                    # show or change settings
aaswap upgrade                   # check for a newer release

aaswap account rename work dev   # give an account a different name
aaswap account disable work      # hold it out of rotation (keeps its login)
aaswap account enable work
aaswap account remove work       # forget aaswap's copy — does NOT log you out
aaswap account remove --all      # forget every account
aaswap account export backup.aaswap
aaswap account import backup.aaswap
aaswap account unclaimed         # credentials aaswap preserved but could not file
aaswap account adopt             # take over a ccswap or claude-swap store

aaswap dir map work              # route this directory to an account
aaswap dir unmap
aaswap dir list
```

Every account-addressing command takes `--provider`.

## Tips

- **Do you need to restart after switching?** Usually not. On **Linux and Windows**, credentials are stored in a file and Claude Code re-reads them whenever that file changes, so the new account takes effect on your next message — no restart needed. On **macOS**, credentials live in the Keychain, which Claude Code caches for about 30 seconds; a running session picks up the switch once that cache expires. Restart Claude Code (or close and reopen the VS Code extension tab) only if you want the change to apply instantly.
- **Continuing sessions after switching:** You can keep using the same Claude Code session after switching — run `aaswap switch` in any terminal and carry on. If you'd prefer a clean start, close and reopen Claude Code (or the VS Code extension tab) and use `--resume` to pick your previous session. Either way, the first message on the new account may use extra usage as its conversation cache rebuilds.

## How it works

- Backs up OAuth tokens and config when you add an account
- Swaps only the account-specific Claude login when you switch accounts;
  live account-independent OAuth state (such as MCP server logins) is
  preserved instead of being overwritten by an account's older snapshot
- Account credentials stored securely using platform-appropriate methods
- A Claude switch holds Claude Code's own credential locks while writing, so a swap never interleaves with a token refresh. A provider that takes no such locks is not made to wait on another tool's
- A switch freshens the target's token first where the provider can refresh one, and quarantines accounts whose refresh token has died (recover by logging in with it and running `aaswap login`, or by replacing its stored credentials from a known-good export — a plain `aaswap account import backup.aaswap` replaces dead-token accounts automatically)
- Usage numbers refresh every few minutes — faster for an account being used or close to switching, slower for idle ones — keeping aaswap comfortably inside Anthropic's rate limits however many dashboards you keep open on a machine. An age note like `· 6m ago` just means the next scheduled check hasn't come yet, not that something is stuck.

## Data locations

| Platform | Credentials | Config backups |
|----------|-------------|----------------|
| macOS | macOS Keychain | `~/.aaswap-backup/` |
| Linux / WSL | File-based (inside the backup directory, under `credentials/`) | `${XDG_DATA_HOME:-~/.local/share}/aaswap/` |
| Windows | File-based (inside the backup directory, under `credentials/`) | `~/.aaswap-backup/` |

### Coming from ccswap or claude-swap

aaswap was renamed from **ccswap**, which was itself ported from
[claude-swap](https://github.com/realiti4/claude-swap). It keeps its **own**
store — a different backup directory and a different Keychain service from
either predecessor.

That is not cosmetic. Both projects stamp the same schema version numbers into
the same file names, and neither can tell the other's numbers from its own; the
Python implementation discards a usage table whose version it does not
recognise. Two independently evolving projects cannot share that namespace, so
the first one to bump a version would silently wipe the other's state.

If you have a store from either, `aaswap list` will point it out and

```bash
aaswap account adopt
```

moves it over. The directory is **moved**, not copied — two stores holding the
same refresh tokens would fight, since refreshing rotates the token and
whichever tool got there first would leave the other reporting a live account as
dead. macOS Keychain items are copied rather than moved, so putting the
directory back restores a working claude-swap install.

Each account's stored files live in `vault/<provider>/<account>/`. Session-mode
profiles (`aaswap run`) live in `sessions/`, and tool preferences in
`settings.json` at the backup directory root.

On Linux/WSL, set `XDG_DATA_HOME` to override the default location.

## Building

```bash
mise install        # provisions the pinned Go and golangci-lint
make build          # ./aaswap
make check          # vet, modern-Go check, lint, tests
make race           # tests under the race detector
```

The `make check` target is what CI runs. `go fix -diff ./...` is part of it:
Go 1.27 carries the modernizers inside `go fix`, so the tree cannot drift into
pre-1.27 idioms without failing the build.

## Advanced

### Configuration

Tool preferences live in `settings.json` in the backup root; `aaswap config` reads and edits it with validation, so you never have to find the file or guess valid ranges.

<details>
<summary>Commands & usage</summary>

```bash
aaswap config                              # list effective settings ("(default)" = not set)
aaswap config get autoswitch.threshold
aaswap config set autoswitch.threshold 80  # the listing flags an account at this pct
aaswap config set autoswitch.model Fable   # fold these models' weekly limits in; Fable,Opus for several
aaswap config unset autoswitch.threshold   # back to the default
aaswap config list                         # also prints where settings.json lives

# Three keys, and every one of them changes what you see. The section is still
# called autoswitch because renaming it would migrate everyone's settings.json;
# the six keys that configured the rotation loop went with the loop.
```

`aaswap config --help` lists every key with its valid range and default. Hand-editing the file still works — `aaswap config` is just a safer front door. `list` and `get` take `--json` for scripting.

</details>

### Backup and migration

Move account data between machines or back it up:

```bash
aaswap account export backup.aaswap                    # All accounts to a file
aaswap account export backup.aaswap --account work        # One account
aaswap account export backup.aaswap --full             # Include full ~/.claude.json and credential object (same-PC backup)
aaswap account import backup.aaswap                    # Skips accounts that already exist
aaswap account import backup.aaswap --force            # Overwrite existing
```

The export file is plaintext JSON and, by default, carries only each account's own login — machine-shared MCP/plugin OAuth tokens and the device token stay on the source machine (`--full` keeps everything, for same-PC backups). If you need encryption, pipe through your tool of choice (e.g. `aaswap account export - | gpg -c > backup.gpg`).

If an imported account is the one you're currently logged in as, activate the imported credentials with `aaswap switch N --force` (a plain `switch` to the current account is a safe no-op and won't touch the import).

### JSON output for scripting

Add `--json` to `list`, `status`, or `switch` to emit a single machine-readable JSON object on stdout (human-readable notices go to stderr). Useful for scripting auto-swap and quota tracking.

```bash
aaswap list --json                   # all accounts with usage/quota
aaswap status --json                 # current active account
aaswap switch work --json            # switch, then report the result
aaswap switch 2 --json
```

<details>
<summary>Example output & schema notes</summary>

```json
{
  "schemaVersion": 1,
  "activeAccountNumber": 2,
  "accounts": [
    { "number": 2, "email": "you@example.com", "active": true, "usageStatus": "ok",
      "usage": { "fiveHour": { "pct": 25.0, "resetsAt": "2026-06-22T23:29:59Z" },
                 "sevenDay": { "pct": 16.0, "resetsAt": "2026-06-26T17:59:59Z" } } }
  ]
}
```

Every payload carries a `schemaVersion` (currently `1`); on a handled error stdout is `{"schemaVersion":1,"error":{...}}` with a non-zero exit code. `--switch`/`--switch-to` report `{"switched": true|false, "from": …, "to": …, "reason": …}`.

Usage is served from a per-account cache: when the usage API is briefly unreachable, the last-known numbers are shown instead of nothing (the human view marks them with their age, e.g. `· 2m ago`). Rows with decision-trusted usage carry additive `usageFetchedAt`/`usageAgeSeconds` fields telling you how old the measurement is. Whenever `usage` is null but a last-known measurement exists — data too old to drive a decision (`usageStatus` stays `unavailable`), or a row in a non-`ok` state such as `token_expired` — additive `lastGoodUsage`/`lastGoodFetchedAt`/`lastGoodAgeSeconds` fields preserve the human display without making the account actionable. These fields apply to list rows and the managed active row from `status --json`. An account held out of rotation with `aaswap account disable` carries an additive `"disabled": true` on its row (absent otherwise).

An account row carries its `name` — the handle `switch` and `run` take.

Weekly windows (`sevenDay` and per-model `scoped` entries — never `fiveHour`) additively carry pace fields once the week is ~a day old: `expectedPct` (where usage would sit if spread evenly across the week) and `aheadOfPace` (`true` when meaningfully above that — the same signal the human views show as an `(ahead)`/`(ahead of pace)` marker). `projectedExhaustionAt`/`willLastToReset` extrapolate the current rate into an ETA to 100% and a yes/no "will it last to the reset"; they stay `--json`-only since a linear projection is too rough to present as fact in the UI.

</details>

`aaswap doctor --json` reports every provider's capabilities, each with a
`supported` flag and — when it is false — a `reason` naming what is missing. A
wrapper deciding whether to offer `run` for a provider should read that rather
than parse prose.

### Adding a provider

A provider is a declaration, not a port. The required part is three fields:

```go
provider.Spec{
    Name:  "grok",
    Home:  provider.Home{Env: "GROK_HOME", Default: ".grok"},
    Files: []provider.File{{Path: "auth.json", Role: provider.RoleSecret}},
}
```

That much makes `list`, `status`, `switch`, `login`, `account rename`,
`account remove` and `export`/`import` work. Accounts get named by a digest of
the credential — `aaswap account rename a1b2c3d4 work` renames one — because
nothing has to parse the token format for the tool to be manageable.

Everything beyond those three fields is a capability. Declare `Identity` to read
real addresses out of the credential, `Session` for `run`, `Usage` for quota
reporting, `Refreshable` when tokens can be renewed without a browser. Whatever
is not declared is reported as unsupported by `aaswap doctor`, with a reason —
never silently skipped, and never guessed at.

Files carry a role. `RoleSecret` is the token, `RoleIdentity` is where an
account's name comes from, and `RoleMachine` belongs to the machine rather than
the account — a model choice or an MCP server list. Machine-scoped files are
never swapped, because carrying one account's onto another is a silent
misconfiguration rather than a visible failure.

The design and its reasoning are in [docs/PROVIDERS.md](docs/PROVIDERS.md).

### Add an account from a raw token or API key

If you only have a long-lived setup-token (e.g., produced by `claude setup-token`)
or a managed API key (`sk-ant-api...`) and you don't want to log in via the browser
flow first — useful on headless servers or when receiving a token from another
machine — register it directly. The token type is auto-detected:

```bash
aaswap login --token sk-ant-oat01-...          # OAuth setup-token
aaswap login --token sk-ant-api03-...          # managed API key
aaswap login --token sk-ant-oat01-... --name ci
aaswap login --token -                         # read the token from stdin
aaswap login --token - --email user@example.com  # optional label override
```

`--email` is optional; omitted values are derived from the account's name,
which itself comes from the token's kind — `setup-token@token.local`, or
`api-key@token.local` for API keys. No Anthropic API calls are made.

The dashboard has the same thing behind `t`, which masks everything past the
`sk-ant-oat01-` / `sk-ant-api03-` prefix and names the kind it detected.

**API-key accounts.** An `sk-ant-api...` value registers a managed API-key account
(the kind Claude Code uses after `/login` with a key) rather than an OAuth
setup-token. It switches like any other account; since API keys have no subscription
quota, they show no usage and the usage-aware `switch` strategies never skip them as
rate-limited.

## Uninstall

Remove all data:

```bash
aaswap account remove --all
```

Then remove the binary:

```bash
brew uninstall --cask aaswap   # if installed from the tap
rm "$(command -v aaswap)"      # if installed any other way
```

## Requirements

- Claude Code, installed and logged in

That is the whole list. aaswap is a static binary with no runtime dependency —
on macOS it shells out to the system `security` command for Keychain access,
which is deliberate: Claude Code only trusts a Keychain item whose creator
matches the reader, so linking the framework directly would make macOS prompt
for permission every time the binary changed.

## License

MIT
