# AGENTS.md

Instructions for coding agents working on **ccswap** — a Go 1.27 CLI that
switches a machine between multiple Claude Code logins.

This tool holds the user's live authentication. A bug here does not produce a
wrong answer; it produces a person who cannot log in. Read the invariants below
before changing anything under `internal/credstore`, `internal/keychain`, or
`internal/swap`.

---

## Commands

```bash
make check     # vet + modernize + lint + test — run this before calling work done
make build     # -> ./ccswap
make test      # go test ./...
make race      # go test -race ./...
make modernize # go fix -diff ./...  (non-zero exit when the tree is not modern Go)
make lint      # golangci-lint run
```

`make check` is the gate. `go fix -diff` failing is a build failure, not a
suggestion.

---

## Invariants

These are contracts with software and state that already exist on users'
machines. Breaking one is not a refactor, it is a regression.

1. **On-disk formats are append-only.** `sequence.json`, `settings.json`,
   `autoswitch_state.json`, `credentials/*`, `*.enc`, `.ccswap` export files and
   session-profile manifests are all read by ccswap versions the user may still
   have installed. Add fields; never rename or remove them. Unknown fields must
   survive a read-modify-write round trip — that is what the
   `Extra map[string]jsontext.Value` + `json:",embed"` fallback is for.

2. **macOS Keychain access shells out to `/usr/bin/security`, by absolute
   path.** Not `os/exec.LookPath`, not `Security.framework` via cgo. Claude Code
   created these items and the Keychain's creator-equals-reader rule is the only
   reason the user is not prompted on every read. The wire details are part of
   the contract too: `-X` hex encoding, secret over stdin with `-i`, exit code
   44 means *not found* (not *error*), 5 second timeout.

3. **`CGO_ENABLED=0`.** The point of the Go port is one file a user can copy
   onto a machine with no toolchain. No dependency may require cgo.

4. **Claude Code's advisory locks are respected.** Take the same locks on the
   same files, or a token refresh and a swap will interleave and one of them
   will lose.

5. **`--json` output is `schemaVersion: 1` and additive-only.** Scripts consume
   it. New keys are fine; changed or removed keys are not.

6. **The CLI surface is stable in both spellings** — the verb subcommands
   (`ccswap switch`) and the legacy flags (`ccswap --switch`).

7. **Tests never touch real state.** Three guards panic in test builds when the
   real thing is reached: `paths.guardRealStore`, `keychain.guardRealKeychain`,
   `claudeapi.guardRealEndpoint`. They replace Python's `sys.addaudithook`
   safety net and are the reason this codebase injects a dependency at every
   I/O boundary. Do not weaken a guard to make a test pass — inject a fake.

---

## Go style: write modern Go

Target the full Go 1.27 feature set (`go.mod` says `go 1.27.0`). Two failure
modes to actively resist: reaching for a pre-generics idiom because it is more
common in training data, and not knowing about a stdlib addition at all.

The canonical, maintained source is JetBrains' **Modern Go Guidelines**
(<https://github.com/JetBrains/go-modern-guidelines>). Install it once and the
agent gets version-aware `list`/`explain` lookups:

```
/plugin marketplace add JetBrains/go-modern-guidelines
/plugin install modern-go-guidelines@goland-claude-marketplace
```

The rules for Go 1.27 are reproduced below so this file stands on its own
without the plugin. They are ordered newest first; **all of them apply**, not
just the recent ones.

- `generic_methods` — Use generic methods instead of package-level generic helper functions when the operation naturally belongs to the type itself.
- `json_v2` — Use `encoding/json/v2` for new JSON code in Go 1.27+; leave existing `encoding/json` code unchanged unless migration is explicitly requested.
- `promoted_field_literals` — Set embedded struct fields directly with promoted field names in Go 1.27+ struct literals instead of constructing the embedded struct explicitly.
- `strings_bytes_cut_last` — Use `strings.CutLast` and `bytes.CutLast` instead of `LastIndex` plus manual slicing around the last separator.
- `stdlib_uuid` — Use the standard library `uuid` package instead of third-party libraries or custom UUID implementations when targeting Go 1.27+.
- `url_clone` — Use the `URL.Clone` and `Values.Clone` methods from `net/url` to copy URLs and `URL` values instead of manual copying.
- `new_expression` — Use `new(value)` for pointer fields or arguments instead of generic/type-specific pointer helper functions or temporary variables used only for `&value`.
- `errors_as_type` — Use `errors.AsType[T](err)` when checking whether an error matches a specific type.
- `sync_waitgroup_go` — Use `wg.Go` when spawning goroutines tracked by a `sync.WaitGroup`.
- `testing_t_context` — Use `t.Context()` when a test function needs a context tied to the test lifetime.
- `json_omitzero` — Use `omitzero` on JSON-tagged bool, numeric, struct, and time fields whose zero value should be omitted; keep `omitempty` for empty strings, slices, and maps.
- `testing_b_loop` — Use `b.Loop()` for the main loop in benchmark functions.
- `strings_split_seq` — Use `strings.SplitSeq`, `strings.FieldsSeq`, `bytes.SplitSeq`, or `bytes.FieldsSeq` when iterating over split results.
- `maps_keys_values_iter` — Use `maps.Keys` or `maps.Values` directly as iterators instead of manually looping over a map.
- `slices_collect` — Use `slices.Collect` to build a slice from an iterator.
- `slices_sorted` — Use `slices.Sorted` to collect and sort iterator values in one step.
- `time_tick_gc` — Use `time.Tick` when it fits; Go 1.23 can recover unreferenced tickers without requiring `Stop` for GC.
- `range_over_int` — Use `for i := range n` when iterating from `0` to `n-1`.
- `loopvar_capture` — Do not add redundant loop-variable copies before closures or taking addresses; Go 1.22 gives each iteration its own variables.
- `cmp_or` — Use `cmp.Or` to pick the first non-zero value from a fallback chain.
- `reflect_type_for` — Use `reflect.TypeFor[T]()` instead of `reflect.TypeOf((*T)(nil)).Elem()`.
- `http_servemux_patterns` — Use method-aware `ServeMux` patterns and `r.PathValue` for path parameters.
- `min_max` — Use built-in `min` and `max` instead of handwritten comparisons.
- `clear` — Use `clear(m)` to delete all map entries or `clear(s)` to zero slice elements.
- `slices_contains` — Use `slices.Contains` instead of a manual search loop.
- `slices_index` — Use `slices.Index` to find the index of an element, returning `-1` when absent.
- `slices_index_func` — Use `slices.IndexFunc` to find an element by predicate.
- `slices_sort_func` — Use `slices.SortFunc` with `cmp.Compare` instead of `sort.Slice` for typed comparisons.
- `slices_sort` — Use `slices.Sort` for slices of ordered values.
- `slices_max_min` — Use `slices.Max` and `slices.Min` instead of manual loops over ordered values.
- `slices_reverse` — Use `slices.Reverse` instead of a manual swap loop.
- `slices_compact` — Use `slices.Compact` to remove consecutive duplicates in place.
- `slices_clip` — Use `slices.Clip` to remove unused capacity.
- `slices_clone` — Use `slices.Clone` to copy a slice.
- `maps_clone` — Use `maps.Clone` instead of manual map iteration.
- `maps_copy` — Use `maps.Copy` to copy entries from one map into another.
- `maps_delete_func` — Use `maps.DeleteFunc` to delete map entries that match a predicate.
- `sync_once_func` — Use `sync.OnceFunc` instead of `sync.Once` plus a wrapper closure.
- `sync_once_value` — Use `sync.OnceValue` to memoize a computed value.
- `context_after_func` — Use `context.AfterFunc` to run cleanup when a context is canceled.
- `context_timeout_deadline_cause` — Use timeout and deadline contexts with causes when callers need to inspect the cancellation reason.
- `bytes_clone` — Use `bytes.Clone` to copy a byte slice.
- `strings_cut_prefix_suffix` — Use `strings.CutPrefix` or `strings.CutSuffix` when you need both the trimmed result and whether it matched.
- `errors_join` — Use `errors.Join` to combine multiple errors while preserving error matching.
- `context_cancel_cause` — Use `context.WithCancelCause` and `context.Cause` when cancellation needs to carry an error cause.
- `fmt_appendf` — Use `fmt.Appendf` when appending formatted text to a byte slice and an intermediate `fmt.Sprintf` string is unnecessary.
- `atomic_types` — Use typed atomics such as `atomic.Bool`, `atomic.Int64`, and `atomic.Pointer[T]` instead of untyped atomic functions.
- `any` — Use `any` instead of `interface{}`.
- `bytes_cut` — Use `bytes.Cut` instead of `bytes.Index` plus manual slicing.
- `strings_clone` — Use `strings.Clone` to copy a string without retaining shared backing memory.
- `strings_cut` — Use `strings.Cut` instead of `strings.Index` plus manual slicing.
- `errors_is` — Use `errors.Is(err, target)` instead of `err == target` so wrapped errors are handled correctly.
- `time_until` — Use `time.Until(deadline)` instead of `deadline.Sub(time.Now())`.
- `time_since` — Use `time.Since(start)` instead of `time.Now().Sub(start)`.

### Where the guidelines and this repo already agree

`make modernize` (`go fix -diff`) enforces the mechanical subset automatically.
The rules it cannot check are the ones to watch: `json_v2`, `errors_as_type`,
`stdlib_uuid`, `generic_methods`, `promoted_field_literals`, `json_omitzero`,
`atomic_types`, `sync_once_value`, `context_after_func`.

Concretely, in this codebase:

- JSON is `encoding/json/v2` + `jsontext` everywhere. Import it as
  `json "encoding/json/v2"`.
- `omitzero`, not `omitempty`, on bool/numeric/struct/time fields.
- Error matching is `errors.Is` against the `internal/apperr` sentinel tree
  (`apperr.Err` → `ErrCredential` → `ErrCredentialRead`, …), and
  `errors.AsType[T]` when a concrete type is needed. Never `==`, never a bare
  type assertion — `errorlint` will catch it, but write it right the first time.
- Concurrent usage fetches use `sync.WaitGroup.Go`, not a hand-rolled
  `Add`/`Done` pair.
- Nothing reads the wall clock directly. Every time-dependent type takes a
  `Now func() time.Time` field (or `SetClock`) and tests drive it forward by
  hand, so cooldowns, lease TTLs and backoff are deterministic without
  sleeping. `testing/synctest` is available for goroutine-timing tests if one
  ever needs it, but no test currently does.

---

## Project conventions

**Comments say *why*, at length.** This codebase is unusually comment-dense and
that is deliberate: nearly every rule in it exists because of a specific failure
mode in Claude Code's auth, and a reader who does not know the failure mode will
"simplify" the rule away. Explain the hazard, not the mechanics.

```go
// Last, because it licenses the bytes just read. Ahead of the read, a
// /login landing in between would store a config the check never saw.
if err := s.RejectIdentityDrift(identity); err != nil {
```

Do not write comments that restate the code (`// increment i`). Do not delete an
existing rationale comment while editing the line under it.

**Test names are sentences about behavior**, not about method names:

```go
func TestAnUnchangedCredentialAnnouncesNothing(t *testing.T)
func TestResolvePrefersTheNearestAncestor(t *testing.T)
func TestAnUnusableTableReadsAsEmpty(t *testing.T)
```

Table-driven by default. Each test carries a comment naming the real-world
breakage it prevents. Use `t.Context()`, `t.TempDir()`, `t.Setenv()`.

**Dependencies are injected at every boundary.** A package that reads the
filesystem, the Keychain, the network, or spawns a process takes that capability
as a field or an interface. The storage layer never imports the switcher; it
sees a data-only view. This is what makes the test guards enforceable.

**Destructive work happens last.** Read, verify, and hold everything in memory
first; only then mutate. Multi-step mutations record explicit rollback steps and
unwind in reverse. See `swap/add.go` and `swap/activate.go` for the shape.

**Third-party dependencies need a reason.** The whole tree is stdlib plus
Bubble Tea v2 / Lipgloss v2 (TUI), Cobra (CLI), and `golang.org/x/{sys,term,text}`.
Do not add a library for something the stdlib does.

---

## Layout

```
cmd/ccswap/                                       entrypoint, kept thin
internal/
  platform/ paths/ fsutil/ lockfile/ apperr/      foundation
  settings/ buildinfo/
  keychain/ credstore/                            credential storage
  claudeapi/ usage/ usagestore/ pollpolicy/ pace/ usage pipeline
  swap/                                           the switcher, split by role
  session/ mappings/ transfer/ procdetect/        session mode, data movement
  autoswitch/                                     policy engine
  cli/ render/ jsonout/ tui/ updatecheck/         surfaces
  testutil/                                       cross-platform test helpers
```

`internal/swap` is split by role on purpose — `add.go`, `activate.go`,
`relocate.go`, `remove.go`, `snapshot.go`, `report.go`, `roster.go`,
`identity.go`. Put new switcher behavior in the file that owns that role rather
than growing one of them into a god object again.
