# ccswap

Multi-account switcher for Claude Code. Switch between Claude accounts without
logging out, or let it switch for you before you hit a rate limit. See every
account's remaining quota at a glance, and run several accounts in parallel.
Works with both the Claude Code CLI and the VS Code extension.

A single static binary — no runtime to install, on macOS, Linux or Windows.

## Installation

ccswap is a single static binary. It needs no runtime, no interpreter, and no
package index.

### Homebrew

```bash
brew install d0lim/tap/ccswap
```

### Go

```bash
go install github.com/d0lim/ccswap/cmd/ccswap@latest
```

### Download a binary

Grab the build for your platform from the [releases page](https://github.com/d0lim/ccswap/releases),
unpack it, and put `ccswap` somewhere on your `PATH`.

### From source

```bash
git clone https://github.com/d0lim/ccswap.git
cd ccswap
make build          # produces ./ccswap
```

### Updating

```bash
ccswap upgrade       # reports the latest release and the command for your install
```

`ccswap upgrade` does not replace the binary in place. A binary installed by a
package manager belongs to that package manager, and swapping it out from
underneath would leave its records wrong — so ccswap tells you the command
instead of guessing.

## Usage

### Add your first account

Log into Claude Code with your first account, then:

```bash
ccswap add
```

### Add more accounts

Log in with another account, then:

```bash
ccswap add
```

Do not run `/logout` first: current Claude Code may revoke the refresh token stored for the account you are leaving.

### Switch accounts

Rotate to the next account:

```bash
ccswap switch
```

Or switch to a specific account:

```bash
ccswap switch 2
ccswap switch user@example.com
ccswap switch dev                # or by alias, once set with `ccswap alias 2 dev`
```

Not sure which one? `ccswap list` shows every account's 5-hour and 7-day usage and reset times at a glance:

```bash
ccswap list
```

### The dashboard

`ccswap tui` is the same information as an interactive screen: usage bars per
account, reset times, and switching without retyping a slot number.

```bash
ccswap tui
```

```
 ccswap                                                     watch on

 ▸ ● 1 work@example.com  (work)                                  Acme
     5h  ██████████████▉░░░░░░░░░  62%   resets 20:39
     7d  ███████▍░░░░░░░░░░░░░░░░  31%   resets Jul 5 22:12

   ○ 2 spare@example.com                                     personal
     5h  ██▋░░░░░░░░░░░░░░░░░░░░░  11%   resets 18:02
     7d  ████▌░░░░░░░░░░░░░░░░░░░  19%   resets Jul 7 12:12

   ↑↓ move  ·  enter switch  ·  d disable  ·  r refresh  ·  w watch  ·  q quit
```

`w` turns on watch mode, which re-collects every 30 seconds. Switching asks
before it replaces a live credential. It needs a terminal — in a script, use
`ccswap list --json`.

Or let ccswap auto-pick by remaining quota — `ccswap switch --strategy best` (most quota left) or `--strategy next-available` (skip rate-limited accounts).

**Note:** You usually don't need to restart — on Linux/Windows the new account is picked up automatically, and on macOS after the Keychain cache expires. To apply it instantly, restart Claude Code or reopen the VS Code extension tab. See [Tips](#tips) for the per-platform details.

### Automatic switching

Let ccswap watch your usage and switch for you. When the active account's 5-hour or 7-day window reaches the threshold (default 90%), it switches to the account with the most quota left — before you hit the limit, and safe to run while Claude Code is working:

```bash
ccswap auto                     # foreground loop, polls every 60s
ccswap auto --threshold 80      # switch earlier
ccswap auto --model Fable       # also switch when the Fable weekly limit is hit
ccswap auto --once              # single check-and-switch, for cron/scripts
ccswap auto --dry-run           # log what it would do, never switch
ccswap auto --strategy consume-first   # burn the soonest-resetting account first
```

<details>
<summary>How it behaves & advanced usage</summary>

- Runs safely alongside Claude Code: switches take the same credential locks Claude Code uses, so a swap never collides with a token refresh.
- A cooldown (default 5 min) and a hysteresis margin stop it flip-flopping near the threshold: a proactive switch only lands on an account that's below the threshold *and* better than the current one by the margin — a candidate that clears the margin is always taken, but two accounts hovering at the line never ping-pong. When every account is exhausted it keeps checking on a bounded slow cadence, waking sooner for an imminent reset.
- **Strategies** (`--strategy`, or `ccswap config set autoswitch.strategy`): `best` (default) stays put until the active account nears its limit, then moves to the account with the most quota left. `consume-first` proactively keeps you on the account whose **weekly window resets soonest** — use-it-or-lose-it — switching to a sooner-resetting account (with room to spare) even below the threshold, so perishable weekly quota isn't wasted.
- Usage polling is adaptive — a couple of accounts per check, busy alternates watched more closely, and exhausted ones checked about every ten minutes (or slower after 429s) — so API traffic stays flat no matter how many accounts you manage.
- It fails safe: if a usage check errors it keeps trusting the last-known numbers while retries back off, and an expired token on an idle machine makes it hold rather than fail over (Claude Code refreshes the token on your next message).
- An account whose refresh token has died is quarantined and reported until you either log in with it and re-run `ccswap add --slot N`, or replace its stored credentials from a known-good export — a plain `ccswap import backup.ccswap` replaces dead-token slots on its own (`--force` is still required to replace other existing accounts; note a stale export can carry an already-superseded token). API-key accounts are never rotated onto unless you pass `--include-api-key-accounts`.
- To hold an account out of rotation yourself — a work account you don't want touched, one you're resting — run `ccswap disable <num|email>`; `ccswap enable <num|email>` puts it back. Disabled accounts are skipped by auto-switch, bare `ccswap switch`, and the `best` / `next-available` strategies, but stay fully managed and remain a valid explicit `ccswap switch <num|email>` target. They show an `(out of rotation)` marker in `ccswap list`.
- By default only the account-wide 5h/7d windows drive switching. If you work on one model and hit its **weekly per-model limit** first (e.g. Fable), add `--model Fable` (or `ccswap config set autoswitch.model Fable`) to fold that model's window into the decision, so it switches off an account whose model quota is spent even while its 5h/7d windows still have room.
  - **Model names** are Anthropic's own per-model `display_name`s, matched case-insensitively. The exact strings for your accounts are the per-model rows in `ccswap list` (e.g. a line reading `Fable: 100%`).

For cron/systemd timers, `--once` reports the outcome in its exit code (`0` switched, `1` error, `2` nothing to do, `3` blocked — no viable target), and `--json` emits one JSON event per line:

```bash
*/5 * * * * ccswap auto --once --json >> ~/.ccswap-auto.log 2>&1
```

Defaults like the threshold and cooldown are configurable with `ccswap config set autoswitch.threshold 80` — flags override them (see [Configuration](#configuration)).

</details>

### Run multiple accounts at the same time (session mode)

Launch Claude Code as a specific account in the current terminal only — every other terminal and the VS Code extension stay on your default account, so two accounts can work in parallel.

```bash
ccswap run 2                     # launch Claude Code as account 2, here only
ccswap run user@example.com      # by email
ccswap run 2 -- --resume         # everything after '--' is forwarded to claude
ccswap run 2 --share-history     # share your chat history with this account too
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

Bind a directory to an account, and a bare `ccswap run` there launches that account in session mode — e.g. work account in work repos, personal elsewhere:

```bash
ccswap map 2 ~/work/client-app   # map a directory to account 2
ccswap map user@example.com      # map the current directory
ccswap map                       # list mappings
ccswap unmap ~/work/client-app   # remove one (defaults to current directory)

cd ~/work/client-app/src
ccswap run                       # → account 2, session mode
```

Subfolders inherit the nearest mapped ancestor. In an unmapped directory, `ccswap run` just launches plain `claude` with your default login. Mappings are per-machine (not part of `ccswap export`) and are cleaned up when their account is removed.

</details>

### Refresh expired tokens

If an account's token expires, log back into Claude Code with that account and re-run:

```bash
ccswap add
```

This will update the stored credentials without creating a duplicate.

### Other commands

```bash
ccswap run 2                     # Run an account in this terminal only (session mode)
ccswap auto                      # Auto-switch when nearing rate limits (see above)
ccswap config                    # Show or edit settings (see Configuration below)
ccswap list                      # Show all accounts with 5h/7d usage and reset times
ccswap list --token-status       # Add source-labelled OAuth token diagnostics
ccswap status                    # Show current account
ccswap add --slot 3              # Add account to a specific slot (prompts before overwrite)
ccswap add --alias dev           # Add account and give it a short alias
ccswap remove 2                  # Remove an account
ccswap disable 2                 # Hold an account out of auto-rotation (keeps its login)
ccswap enable 2                  # Return a disabled account to rotation
ccswap alias 2 dev               # Give an account a short alias (usable anywhere NUM|EMAIL is)
ccswap alias 2 --unset           # Remove an account's alias
ccswap alias                     # List all aliases
ccswap move 2 1                  # Assign an account to a slot (relocates to an empty slot, swaps if taken)
ccswap unclaimed                 # List stashed credential entries (slot + why they were stashed)
ccswap unclaimed --purge ID      # Drop one (deletes its bytes; recover with /login + `ccswap add`)
ccswap upgrade                   # Upgrade ccswap to the latest version
ccswap purge                     # Remove all ccswap data
```

The original flag spellings (`ccswap --switch`, `ccswap --list`, ...) keep working.

## Tips

- **Do you need to restart after switching?** Usually not. On **Linux and Windows**, credentials are stored in a file and Claude Code re-reads them whenever that file changes, so the new account takes effect on your next message — no restart needed. On **macOS**, credentials live in the Keychain, which Claude Code caches for about 30 seconds; a running session picks up the switch once that cache expires. Restart Claude Code (or close and reopen the VS Code extension tab) only if you want the change to apply instantly.
- **Continuing sessions after switching:** You can keep using the same Claude Code session after switching — run `ccswap switch` in any terminal and carry on. If you'd prefer a clean start, close and reopen Claude Code (or the VS Code extension tab) and use `--resume` to pick your previous session. Either way, the first message on the new account may use extra usage as its conversation cache rebuilds.

## How it works

- Backs up OAuth tokens and config when you add an account
- Swaps only the account-specific Claude login when you switch accounts;
  live account-independent OAuth state (such as MCP server logins) is
  preserved instead of being overwritten by a slot's older snapshot
- Account credentials stored securely using platform-appropriate methods
- Switches (manual and automatic) hold Claude Code's own credential locks while writing, so a swap never interleaves with a token refresh
- Auto-switch freshens a target's token before activating it, and quarantines accounts whose refresh token has died (recover by re-adding it with `ccswap add --slot N`, or by replacing its stored credentials from a known-good export — a plain `ccswap import backup.ccswap` replaces dead-token slots automatically)
- Usage numbers refresh every few minutes — faster for an account being used or close to switching, slower for idle ones — keeping ccswap comfortably inside Anthropic's rate limits however many dashboards you keep open on a machine. An age note like `· 6m ago` just means the next scheduled check hasn't come yet, not that something is stuck.

## Data locations

| Platform | Credentials | Config backups |
|----------|-------------|----------------|
| macOS | macOS Keychain | `~/.ccswap-backup/` |
| Linux / WSL | File-based (inside the backup directory, under `credentials/`) | `${XDG_DATA_HOME:-~/.local/share}/ccswap/` |
| Windows | File-based (inside the backup directory, under `credentials/`) | `~/.ccswap-backup/` |

### Coming from claude-swap

ccswap was forked from [claude-swap](https://github.com/realiti4/claude-swap)
and keeps its **own** store — a different backup directory and a different
Keychain service.

That is not cosmetic. Both projects stamp the same schema version numbers into
the same file names, and neither can tell the other's numbers from its own; the
Python implementation discards a usage table whose version it does not
recognise. Two independently evolving projects cannot share that namespace, so
the first one to bump a version would silently wipe the other's state.

If you already use claude-swap, `ccswap list` will point out the store and

```bash
ccswap import-store
```

moves it over. The directory is **moved**, not copied — two stores holding the
same refresh tokens would fight, since refreshing rotates the token and
whichever tool got there first would leave the other reporting a live account as
dead. macOS Keychain items are copied rather than moved, so putting the
directory back restores a working claude-swap install.

Session-mode profiles (`ccswap run`) live under the backup directory in `sessions/`. Tool preferences (`settings.json`) and auto-switch state (`autoswitch_state.json` — cooldown and quarantined accounts; delete it to reset) live in the backup directory root.

On Linux/WSL, set `XDG_DATA_HOME` to override the default location.

## Building

```bash
mise install        # provisions the pinned Go and golangci-lint
make build          # ./ccswap
make check          # vet, modern-Go check, lint, tests
make race           # tests under the race detector
```

The `make check` target is what CI runs. `go fix -diff ./...` is part of it:
Go 1.27 carries the modernizers inside `go fix`, so the tree cannot drift into
pre-1.27 idioms without failing the build.

## Advanced

### Configuration

Tool preferences live in `settings.json` in the backup root; `ccswap config` reads and edits it with validation, so you never have to find the file or guess valid ranges.

<details>
<summary>Commands & usage</summary>

```bash
ccswap config                              # list effective settings ("(default)" = not set)
ccswap config get autoswitch.threshold
ccswap config set autoswitch.threshold 80  # validated: rejects out-of-range values loudly
ccswap config set autoswitch.model Fable   # per-model switching (see "auto"); Fable,Opus for several
ccswap config unset autoswitch.threshold   # back to the default
ccswap config path                         # where settings.json lives
```

`ccswap config --help` lists every key with its valid range and default. Hand-editing the file still works — `ccswap config` is just a safer front door. `list` and `get` take `--json` for scripting.

</details>

### Backup and migration

Move account data between machines or back it up:

```bash
ccswap export backup.ccswap                    # All accounts to a file
ccswap export backup.ccswap --account 2        # One account
ccswap export backup.ccswap --full             # Include full ~/.claude.json and credential object (same-PC backup)
ccswap import backup.ccswap                    # Skips accounts that already exist
ccswap import backup.ccswap --force            # Overwrite existing
```

The export file is plaintext JSON and, by default, carries only each account's own login — machine-shared MCP/plugin OAuth tokens and the device token stay on the source machine (`--full` keeps everything, for same-PC backups). If you need encryption, pipe through your tool of choice (e.g. `ccswap export - | gpg -c > backup.gpg`).

If an imported account is the one you're currently logged in as, activate the imported credentials with `ccswap switch N --force` (a plain `switch` to the current account is a safe no-op and won't touch the import).

### JSON output for scripting

Add `--json` to `list`, `status`, or `switch` to emit a single machine-readable JSON object on stdout (human-readable notices go to stderr). Useful for scripting auto-swap and quota tracking.

```bash
ccswap list --json                   # all accounts with usage/quota
ccswap status --json                 # current active account
ccswap switch --strategy best --json # switch, then report the result
ccswap switch 2 --json
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

Usage is served from a per-account cache: when the usage API is briefly unreachable, the last-known numbers are shown instead of nothing (the human view marks them with their age, e.g. `· 2m ago`). Rows with decision-trusted usage carry additive `usageFetchedAt`/`usageAgeSeconds` fields telling you how old the measurement is. Whenever `usage` is null but a last-known measurement exists — data too old to drive a decision (`usageStatus` stays `unavailable`), or a row in a non-`ok` state such as `token_expired` — additive `lastGoodUsage`/`lastGoodFetchedAt`/`lastGoodAgeSeconds` fields preserve the human display without making the account actionable. These fields apply to list rows and the managed active row from `status --json`. An account held out of rotation with `ccswap disable` carries an additive `"disabled": true` on its row (absent otherwise).

An account row also carries an additive `alias` field once one is set with `ccswap alias` (e.g. `"alias": "dev"`); accounts without one simply omit the key.

Weekly windows (`sevenDay` and per-model `scoped` entries — never `fiveHour`) additively carry pace fields once the week is ~a day old: `expectedPct` (where usage would sit if spread evenly across the week) and `aheadOfPace` (`true` when meaningfully above that — the same signal the human views show as an `(ahead)`/`(ahead of pace)` marker). `projectedExhaustionAt`/`willLastToReset` extrapolate the current rate into an ETA to 100% and a yes/no "will it last to the reset"; they stay `--json`-only since a linear projection is too rough to present as fact in the UI.

</details>

`ccswap auto --json` emits an event *stream* instead — one JSON object per line (`{"schemaVersion":1,"event":"switch","ts":…, …}` with kinds like `poll`, `switch`, `no-switch`, `quarantine`, `unquarantine`, `all-exhausted`, `sleep`, `error`). The contract is additive: new kinds and fields may appear, so scripts should ignore unknown ones.

### Add an account from a raw token or API key

If you only have a long-lived setup-token (e.g., produced by `claude setup-token`)
or a managed API key (`sk-ant-api...`) and you don't want to log in via the browser
flow first — useful on headless servers or when receiving a token from another
machine — register it directly. The token type is auto-detected:

```bash
ccswap add-token sk-ant-oat01-...             # OAuth setup-token
ccswap add-token sk-ant-api03-...             # managed API key
ccswap add-token sk-ant-oat01-... --slot 3
ccswap add-token - --slot 3                   # read token from stdin
ccswap add-token --email user@example.com     # optional label override
```

`--email` is optional; omitted values use `setup-token-{slot}@token.local`
(or `api-key-{slot}@token.local` for API keys). No Anthropic API calls are made.

**API-key accounts.** An `sk-ant-api...` value registers a managed API-key account
(the kind Claude Code uses after `/login` with a key) rather than an OAuth
setup-token. It switches like any other account; since API keys have no subscription
quota, they show no usage and the usage-aware `switch` strategies never skip them as
rate-limited.

## Uninstall

Remove all data:

```bash
ccswap purge
```

Then remove the binary:

```bash
brew uninstall ccswap    # if installed from the tap
rm "$(command -v ccswap)"      # if installed any other way
```

## Requirements

- Claude Code, installed and logged in

That is the whole list. ccswap is a static binary with no runtime dependency —
on macOS it shells out to the system `security` command for Keychain access,
which is deliberate: Claude Code only trusts a Keychain item whose creator
matches the reader, so linking the framework directly would make macOS prompt
for permission every time the binary changed.

## License

MIT
