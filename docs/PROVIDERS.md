# 프로바이더 아키텍처

여러 에이전트 CLI의 로그인을 하나의 계약으로 관리하기 위한 설계.

대상: Claude Code, Codex, Gemini, Antigravity, Grok, Cursor. **구현은 Claude와
Codex 먼저** — 이 머신에서 실제로 로그인 상태를 확인할 수 있는 둘뿐이고,
나머지는 자격증명 파일 형태를 실측하지 않은 채로 선언을 쓰면 그건 설계가 아니라
추측이다.

참조: [Dicklesworthstone/coding_agent_account_manager](https://github.com/Dicklesworthstone/coding_agent_account_manager)

---

> **구현 완료.** 이 문서는 설계이자 구현 기록이다. 계획과 실제가 갈린 곳은
> §11에 모아 두었다 — 계획이 틀린 이유까지 적혀 있다.

## 0. 확정된 결정

| 항목 | 결정 | 근거 |
|---|---|---|
| `run` (세션 격리) | **유지** | 격리이지 우회가 아니다. §5 |
| `auto` (자동 회전) | **제거** | 설계상 rate limit 우회. §0.1 |
| 저장소 마이그레이션 | **지금, v2 안에서** | v2가 아직 릴리스되지 않았다. §8 |
| 구현 순서 | claude → codex → (나머지) | 실측 가능한 것부터 |

### 0.1 `auto`를 제거하는 이유

`auto`는 계정별 사용량을 폴링해 임계치에서 살아있는 자격증명을 갈아끼운다.
한 머신에서 N개 토큰이 라운드로빈으로 사용량 엔드포인트를 치는 패턴은 정상적인
단일 사용자가 만들 수 없는 지문이고, 계정 A가 90%에서 멈추고 몇 초 뒤 계정 B가
같은 머신에서 시작하는 시계열도 마찬가지다.

나머지 기능은 전부 "계정이 여러 개인 사람이 그걸 정리해서 쓴다"로 방어된다.
회전 하나가 도구 전체의 성격을 규정한다.

함께 사라진 것: `internal/autoswitch/`(8파일), `aaswap auto`,
`switch --strategy`, `switch --model`, `auto --once`를 위해 있던 종료코드
운반자. 총 3,258줄.

**계획과 달리 남긴 것**: `internal/pace/`와 `internal/pollpolicy/`. 둘 다
회전이 아니라 **표시**를 섬긴다 — pace는 `list`에서 주간 창이 페이스보다
앞서는 것을 표시하고, poll policy는 `list`가 엔드포인트를 두드리지 않게 하는
캐던스다. settings의 autoswitch 섹션도 남았다: threshold와 model 키를
`list`가 읽는다. 이름이 아직 "autoswitch"인 것은 이제 오해를 부르지만,
그것만을 위해 settings 마이그레이션을 할 값은 없다.

---

## 1. 계약

```go
// Provider is everything aaswap needs to know about one agent CLI.
//
// Only Name, Home and Files are required. Everything else is a capability: a
// provider that does not declare one is not broken, it is a provider whose
// commands needing that capability report it as unsupported — by name, with a
// reason — instead of silently doing the wrong thing.
type Spec struct {
	Name  string
	Home  Home
	Files []File

	Login    *Login
	Identity IdentitySource // nil → hash fallback (§4)
	Usage    UsageSource    // nil → headroom is unknown, never zero
	Session  *Session       // nil → `run` is unsupported here (§5)
	Hazards  []Hazard       // state that survives a swap (§7)

	// Declared facts rather than implementations: both need something the
	// declaration must stay constructible without — an HTTP client, and a
	// platform-specific store.
	Refreshable bool // false → the only answer to an expired token is `login`
	Keychain    bool // true → the live credential is in TWO stores (§2)
}

// Home is where the tool keeps everything, and how to repoint it.
type Home struct {
	// Env repoints the whole home. Empty when the tool has no such variable,
	// which also means it cannot host a `run` profile.
	Env string
	// Default is relative to the user's home directory: ".codex".
	Default string
	// Outside holds auth files that live outside Home. Claude's ~/.claude.json
	// is the only known case, and designing for it as the common shape is why
	// the current implementation does not generalise.
	Outside []File
}

// File is one path inside a provider's home, and what it belongs to.
type File struct {
	Path     string
	Role     Role
	Optional bool
}
```

### Role — 같은 디렉터리의 파일이 서로 다른 것에 속한다

| Role | 뜻 | 스왑 | 보호 | 예 |
|---|---|---|---|---|
| `RoleSecret` | 토큰 그 자체 | ✅ | 0600, macOS 키체인(선언 시) | `.codex/auth.json` |
| `RoleIdentity` | 계정 이름의 출처 | ✅ | 0600 | `~/.claude.json` |
| `RoleMachine` | 머신에 속하는 설정 | ❌ | — | `.codex/config.toml` |

`RoleSecret`과 `RoleIdentity`는 같은 파일일 수 있다. Codex가 그렇다 —
`auth.json` 안의 `tokens.id_token`이 곧 신원 문서다.

`RoleMachine`은 발명이 아니라 이미 발견한 사실이다. 현재 `ReadLiveConfig`의
주석이 말하는 그대로: *"config.toml holds settings that belong to the machine
rather than to whoever is logged in — swapping those would carry one account's
model choice onto another."* 실제로 이 머신의 `config.toml`에는 `model`,
`service_tier`, `[mcp_servers.*]`가 들어 있다. 계정과 무관하다.

---

## 2. 키체인은 오버레이지 분기가 아니다

현재 구조는 "자격증명은 macOS면 키체인, 아니면 파일"이라는 분기를 코어에
두고 있다. 이건 Claude 하나의 사실이다. Codex·Gemini·Grok은 모든 OS에서 평문
파일이다.

키체인은 `RoleSecret` 파일에 대해 **프로바이더가 선언했을 때만** 켜지는
저장 오버레이가 된다. 선언하지 않은 프로바이더는 OS와 무관하게 파일이고,
그래서 **Codex는 Claude보다 OS 분산이 적다.**

---

## 3. 프로바이더 다섯 개를 계약에 대입한 결과

| CLI | Home (env) | secret | machine | 출처 |
|---|---|---|---|---|
| Codex | `CODEX_HOME` / `~/.codex` | `auth.json` | `config.toml` | 실측 |
| Claude Code | `CLAUDE_CONFIG_DIR` / `~/.claude` | `.credentials.json` + 키체인 | `settings.json` | 실측 |
| Gemini | — / `~/.gemini` | `oauth_creds.json` | `settings.json` | CAAM |
| Antigravity | `GEMINI_HOME` / `~/.gemini` | `antigravity-oauth-token` | — | CAAM |
| Grok | `GROK_HOME` / `~/.grok` | `auth.json` | `config.toml` | CAAM |
| Cursor | — / `~/.cursor` | **미상** | `cli-config.json` | 벤더 문서 |

**Claude가 예외다.** 나머지는 홈 디렉터리 하나에 전부 들어 있고, Claude만 홈
밖에 `~/.claude.json`이 있으며 macOS에서만 키체인을 쓴다. 구현 전 아키텍처가
이 예외를 기준으로 세워져 있던 것이 일반화가 안 되던 원인이었다 — 그래서
`Keychain`이 선언 항목이고, 선언하지 않은 프로바이더는 모든 OS에서 파일
하나다.

**Cursor는 secret 위치가 벤더 문서에도 없다.** 이건 조사 부족이 아니라
설계 요구사항이다 — 모르는 채로도 지원할 수 있어야 한다. §4가 그 답이다.

---

## 4. 신원의 3단계

새 프로바이더를 붙이는 비용은 "계정을 어떻게 구분하는가"가 정한다. 한 방법이
아니라 퇴화하는 세 단계로 만든다.

| 단계 | 방법 | 비용 |
|---|---|---|
| 1. 파싱 | 알려진 필드에서 이메일 (`oauthAccount.emailAddress`, `id_token` 클레임) | 프로바이더마다 코드, 포맷이 바뀌면 깨진다 |
| 2. 질의 | CLI 자신에게 물어본다 (`codex login status`) | argv + 파싱 규칙, 프로세스를 띄운다 |
| **3. 해시** | **`RoleSecret` 파일들의 SHA-256 앞 8자** | **0** |

**3단계가 기본값이다.** 프로바이더 추가의 최소 비용은 홈 디렉터리와 secret
파일 경로 두 줄이고, 그것만으로 `list`·`status`·`switch`·`login`·`export`·
`remove`가 전부 동작한다. 이름은 사용자가 `aaswap account rename a1b2c3d4 work`로
붙인다. 파싱은 나중에 붙이는 개선이지 전제가 아니다.

### 부수 효과 — 수동 전환 감지

현재 "살아있는 계정"은 이메일 비교로 판별한다. 그래서 사용자가 aaswap 밖에서
**같은 이메일로** 재로그인하면 못 잡는다. 토큰은 완전히 다른데 신원은 같아
보인다. 해시 비교는 이걸 잡는다.

### 부수 효과 — `login`이 프로바이더 무관해진다

`AwaitNewLogin`은 로그인을 *띄우지* 않고 사용자가 직접 로그인하기를 기다렸다가
착륙한 것을 포획한다. 기다리는 조건을 "이메일이 바뀔 때까지"에서 "secret 파일의
해시가 바뀔 때까지"로 바꾸면, **토큰 형식을 모르는 프로바이더의 로그인도 포획할
수 있다.**

---

## 5. Session — `run`이 남으므로

```go
// Session is what a provider needs to host an isolated `run` profile.
type Session struct {
	// HomeEnv repoints the tool at a profile directory. Without one there is
	// no way to isolate a session, so Session itself cannot be declared.
	HomeEnv string
	// Argv is what `run` launches.
	Argv []string
	// Share is what a profile mirrors from the default one.
	Share ShareSet
	// Liveness answers "is anything running against this profile". Nil is a
	// legitimate declaration and is handled fail-safe — see below.
	Liveness Liveness
}

type ShareSet struct {
	Customizations []string // settings, skills, commands, agents
	History        []string // conversations
}

// Liveness reports the processes running against a profile directory.
//
// complete is false when a record could not be read. A caller must treat an
// incomplete answer the same as "something is running": the whole point is to
// avoid writing a credential out from under a live session, and an unreadable
// record is not evidence of absence.
type Liveness interface {
	PIDs(profileDir string) (pids []int, complete bool)
}
```

`SharedItems`/`HistoryItems`는 `internal/session`에 Claude의 목록으로
하드코딩돼 있었다. 프로바이더 데이터이므로 선언으로 옮겼다.

### 5.1 Liveness가 nil일 때 — fail-safe로 퇴화한다

이게 Codex `run`을 **liveness 조사 없이도** 안전하게 내보낼 수 있게 하는
설계다.

liveness가 쓰이는 곳은 하나로 수렴한다: *돌아가는 세션 밑에서 자격증명을
덮어쓰지 않기.* 현재 코드에서 그 판단은 이렇게 생겼다.

```go
stale := session.IsStale(sessionDir) && manager.Quiescent(num, email)
```

`Quiescent()`가 false를 반환하면 재시딩을 **하지 않는다**. 즉 false가 이미
보수적인 답이다. 따라서:

> **`Liveness == nil`은 "항상 not quiescent"로 해석한다.**

결과는 이렇다. Codex `run`은 동작하고, 프로필이 stale해도 자동으로 재시딩하지
않으며, aaswap은 "이 프로바이더는 실행 중인 세션을 감지할 수 없어 자동
갱신하지 않습니다. `aaswap --provider codex login`으로 직접 갱신하세요"라고
말한다. **조용히 잘못 동작하는 경로가 없다.**

Codex의 liveness 후보는 `~/.codex/thread-writer-locks/`(스레드별 락 파일)와
`state_5.sqlite`, app-server 데몬이다. 특성이 파악되면 `Liveness`를 채워
넣으면 되고, 그때까지 안전은 nil 기본값이 지킨다.

---

## 6. 능력 × 명령

이 표가 문서가 아니라 코드다. `aaswap doctor`가 이걸 그대로 출력한다.

| 명령 | 요구 능력 | Claude | Codex | 신규 CLI(최소 선언) |
|---|---|---|---|---|
| `list` `status` | — (해시 폴백) | ✅ | ✅ | ✅ |
| `switch` | `Files` | ✅ | ✅ | ✅ |
| `login` | `Files` | ✅ | ✅ | ✅ |
| `account rename/remove/disable/enable` | — | ✅ | ✅ | ✅ |
| `account export` `import` | `Files` | ✅ | ✅ | ✅ |
| `doctor` `config` `upgrade` | — | ✅ | ✅ | ✅ |
| `run` · `dir map/unmap/list` | `Session` | ✅ | ✅ (재시딩 없음) | ❌ 미지원 표기 |
| `list --usage` | `Usage` | ✅ | ⚠️ 활성 계정만 | ❌ 미지원 표기 |

비대칭이 아래 두 줄로 줄어든다. 그리고 그 둘조차 "불가"가 아니라 "이
프로바이더는 `Session`/`Usage`를 선언하지 않았다"로 정확히 설명된다.

### 명령 정리

`run`이 남으므로 `dir`도 남는다. 나머지는 그대로 정리한다.

| 지금 | 제안 |
|---|---|
| `auto`, `switch --strategy` | 제거 (§0.1) |
| `purge` | `account remove --all` |
| `account adopt` | 첫 실행 시 자동 |
| `account unclaimed` | `doctor`로 흡수 |
| `config path` | `config list` 머리에 출력 |
| — | `doctor` 추가 |

25개 → 19개.

---

## 7. Hazard — 스왑을 살아남는 것

CAAM이 발견한 것: Claude의 Agent View 데몬은 세션이 끝나도 살아남아 스왑 후에도
이전 계정으로 동작한다. 그래서 CAAM은 `CLAUDE_CODE_DISABLE_AGENT_VIEW=1`을
주입한다.

**자격증명 파일만 바꿔서는 스왑이 끝나지 않는다는 뜻이고, 이건 Claude만의
버그가 아니라 클래스다.** 이 머신의 `~/.codex/`에 같은 모양이 있다:
`.app-server-state-reconciled-v1`, `state_5.sqlite`, `process_manager/`, `ipc/`.

```go
// Hazard is state that outlives a credential swap.
type Hazard struct {
	// Env is injected to disable the offending feature, when there is one.
	Env []string
	// Purge is removed on switch: caches keyed to the previous account.
	Purge []string
	// Warn names a process that should not be running during a swap.
	Warn string
}
```

선언하지 않으면 그냥 모르는 채로 조용히 반쯤 동작한다. **지금이 그 상태다.**

미해결: Codex의 app-server가 실제로 스왑 후 옛 자격증명을 계속 쓰는지 확인하지
않았다. 각 프로바이더를 붙일 때 Hazard 조사가 체크리스트 항목이어야 한다.

---

## 8. 저장소 — vault, v2 안에서

```
<backup root>/
  sequence.json                    schemaVersion 2, providers로 중첩 (변경 없음)
  vault/<provider>/<name>/         ← 새 레이아웃: 파일 트리 그대로
```

계정 하나가 파일 **하나**라는 전제를 파일 **트리**로 바꾸는 것이다.
현재는 `credentials/<provider>/<name>-<email>.json`이다.

### 왜 지금이 유일하게 싼 시점인가

`internal/swap/schema.go`는 **v0.2.0에 존재하지 않는다.** v2 스키마는 아직
릴리스되지 않았고 현재 브랜치에만 있다. 따라서 vault 레이아웃을 v3로 따로
만들 필요가 없다 — **v2 정의 안에 접어 넣으면 사용자 관점에서 마이그레이션은
v1 → v2 한 번이다.**

이미 존재하는 `EnsureUpgraded`가 자격증명을 옮기고 있으므로, 목적지를 평평한
scoped 파일에서 vault 트리로 바꾸는 변경이지 새 마이그레이션이 아니다.

미루면 v2가 릴리스되고, 그 다음엔 **살아있는 자격증명을 두 번 걷는다.**

### 규율 (v1 → v2에서 확립한 것 그대로)

1. 새 위치에 먼저 쓴다
2. 테이블을 발행한다
3. 그 다음에 옛 사본을 지운다

중간에 죽어도 v1이 온전히 읽히는 상태로 남아야 한다. 순서가 설계다.

---

## 9. Claude / Codex 선언 (구현 대상)

```go
var Claude = Provider{
	Name: "claude",
	Home: Home{
		Env:     "CLAUDE_CONFIG_DIR",
		Default: ".claude",
		// The one provider whose auth reaches outside its home.
		Outside: []File{{Path: ".claude.json", Role: RoleIdentity}},
	},
	Files: []File{
		{Path: ".credentials.json", Role: RoleSecret},
		{Path: "settings.json", Role: RoleMachine, Optional: true},
	},
	Identity: parseJSONField("oauthAccount.emailAddress"), // tier 1
	Refresh:  oauthRefresher,                              // the only provider with one
	Usage:    anthropicUsage,
	Session: &Session{
		HomeEnv: "CLAUDE_CONFIG_DIR",
		Argv:    []string{"claude"},
		Share: ShareSet{
			Customizations: []string{"settings.json", "keybindings.json",
				"CLAUDE.md", "skills", "commands", "agents"},
			History: []string{"projects", "history.jsonl"},
		},
		Liveness: procdetectLiveness, // sessions/*.json + ide/*.lock
	},
	Hazards: []Hazard{{Env: []string{"CLAUDE_CODE_DISABLE_AGENT_VIEW=1"}}},
}

var Codex = Provider{
	Name: "codex",
	Home: Home{Env: "CODEX_HOME", Default: ".codex"},
	Files: []File{
		// One file is both the secret and the identity document.
		{Path: "auth.json", Role: RoleSecret | RoleIdentity},
		{Path: "config.toml", Role: RoleMachine, Optional: true},
	},
	Identity: jwtClaim("tokens.id_token", "https://api.openai.com/auth"), // tier 1
	Refresh:  nil,             // no refresher: an expired token means `login`
	Usage:    rolloutUsage,    // live account only, from session rollouts
	Session: &Session{
		HomeEnv: "CODEX_HOME",
		Argv:    []string{"codex"},
		Share: ShareSet{
			Customizations: []string{"config.toml", "skills", "rules",
				"plugins", "hooks.json"},
			History: []string{"sessions", "history.jsonl", "session_index.jsonl"},
		},
		Liveness: nil, // fail-safe: never auto-reseed (§5.1)
	},
	Hazards: nil, // app-server not yet characterised (§7)
}
```

미검증: Codex의 Share 목록은 `~/.codex/`의 실제 내용에서 추린 것이지, 공유해도
안전한지 확인한 것은 아니다. `config.toml`이 `RoleMachine`이면서 동시에
Customizations에 있는 것은 모순이 아니다 — 스왑되지는 않지만 기본 프로필에서
미러링될 수는 있다. 다만 이 조합은 구현 시 검증이 필요하다.

---

## 10. 구현 결과

| 단계 | 상태 | 커밋 |
|---|---|---|
| 프로바이더 계약 + Role | ✅ | `feat(provider): declare providers instead of branching on them` |
| 선언 조회로 분기 교체 | ✅ | `refactor: read the declaration instead of comparing provider names` |
| 신원 3단계 (해시 폴백) | ✅ | 위 두 커밋 |
| 프로바이더 무관 세션(`run`) | ✅ | `feat(session): run sessions for any provider that declares one` |
| 능력 매트릭스 + `doctor` | ✅ | `feat(cli): report the capability matrix, and prove a provider can be added` |
| vault 레이아웃 | ✅ | `feat(credstore): store an account's files in a directory of its own` |
| `auto` 제거 | ✅ | `feat: remove automatic rate-limit rotation` |
| 명령 정리 | ✅ | `refactor(cli): tidy the command surface` |

`make check`(vet, `go fix -diff`, golangci-lint 0 issues, 전체 테스트) 통과,
`deadcode -test` 비어 있음.

---

## 11. 계획이 틀린 곳

설계는 대부분 맞았지만 다섯 곳이 틀렸다. 구현하면서 드러난 것이므로 기록한다.

**1. `pace`와 `pollpolicy`는 회전이 아니라 표시를 섬긴다.** §0.1은 이 둘이
`auto`와 함께 사라진다고 적었다. 실제로는 `render`와 `jsonout`만 쓰고 있었다.
지우면 `list`가 나빠지고 얻는 안전은 없다.

**2. 파서의 거절은 권위 있는 답이다.** §4는 "파서가 거절하면 해시로 퇴화"로
읽히게 썼다. 그렇게 구현했더니 Codex의 API-key 설치(주소가 정말로 없다)에
OAuth 모양의 신원이 만들어졌고, 상위 계층이 그걸 틀린 경로로 보냈다.
**파서를 선언한 프로바이더가 "없다"고 하면 그건 로그인에 대한 사실**이다.
해시는 파서가 *없는* 프로바이더용이다.

**3. 지문(fingerprint)은 best-effort여야 한다.** Claude는 신원을 config에,
자격증명을 macOS 키체인에 둔다. 파일 읽기로는 secret에 닿지 못하므로 digest가
비는데, 그렇다고 신원이 아닌 것은 아니다.

**4. `account unclaimed`와 `account adopt`는 `doctor`에 흡수할 수 없다.**
§6에 그렇게 적었지만 전자는 `--purge`로 자격증명을 파괴하고 후자는 다른
설치본의 자격증명을 복사한다. 진단 명령이 할 일이 아니다. 실제 문제는
**발견 가능성**이었으므로 `doctor`가 보고만 하고 명령은 남겼다.

**5. vault는 API 변경이 아니라 레이아웃 변경이다.** §8은 "계정 하나가 파일
하나"를 파일 트리로 바꾼다고 했고, 그러면 `ReadAccount`/`WriteAccount`의
호출부 26개(전부 살아있는 자격증명 경로)를 건드려야 하는 것처럼 보였다.
실제로는 경로 함수 두 개만 바꾸면 됐다. 다중 파일 지원은 이제 **추가**로
가능하다.

명령 수는 §6이 예상한 19개가 아니라 **23개**다. 4번 때문에 두 개가 남았고,
`doctor`가 하나 늘었다.

---

## 12. 구현하면서 발견한 프로덕션 버그

전부 `run`에 end-to-end 테스트가 없어서 숨어 있었다. `handOver`가 exec으로
프로세스를 대체해 인프로세스 테스트가 불가능했기 때문인데, `App.HandOver`를
주입 가능하게 만들어 해결했다.

| 버그 | 증상 |
|---|---|
| `Manager.storedIdentity`가 `<root>/configs`를 읽는데 switcher는 `<root>/configs/<provider>`에 쓴다 | **Claude `run`이 전부 실패**했다 — "no stored config backup" |
| `switch`가 config 없는 프로바이더에 config 신원을 요구 (두 경로) | **Codex `switch`가 전혀 동작하지 않았다.** e2e 테스트가 저장과 목록만 하고 활성화를 안 했다 |
| `sessionManager`가 `Spec`을 설정하지 않음 | 모든 프로바이더가 Claude 선언을 씀 |
| `defaultProfileDir`가 항상 `~/.claude`를 미러링 | Codex 프로필에 Claude 설정이 링크됨 |
| `ExecProber`가 무엇을 띄웠든 `claude auth status`를 실행 | Codex 세션 검증이 항상 실패 |
| `NewClaudeProfiles`가 `.credentials.json`을 하드코딩 | Codex 프로필에 Codex가 안 읽는 파일을 시딩 |
| 첫 vault 구현에서 unscoped 스토어의 segment가 `claude`로 해석 | 업그레이드의 마지막 단계가 **방금 쓴 사본을 지운다** |

마지막 것은 이 코드베이스에서 두 번째로 같은 모양이다(이전엔
`Unscoped()`가 `credentials/credentials`를 가리켰다). 그래서 vault 사용
여부를 필드가 아니라 **provider 유무에서 파생**시키고, 시딩 accessor를
경유하지 않고 두 경로가 다름을 직접 단정하는 테스트를 두었다.
