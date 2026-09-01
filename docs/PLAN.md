# aaswap 재설계 — 실행 계획

설계안: https://claude.ai/code/artifact/c5afd591-8667-4599-ab98-0ed50865d7b8

각 phase는 **계획 → 검토 → TDD 구현 → `make check` 통과**로 진행한다.

---

## Phase 1 — 개명 + 단일 저장소 마이그레이션 ✅ 완료

살아있는 refresh token 위를 한 번만 지나가기 위해 셋을 묶는다.

### 1a. 기계적 개명
- `go.mod` 모듈 경로, 전체 import
- `cmd/ccswap/` → `cmd/aaswap/`, 바이너리명, Makefile, `.goreleaser.yaml`
- 온디스크 상수 8개: `BackupDirName`, `LegacyBackupDirName`, `BackupService`,
  `LockFileName`, 세션 마커 4종
- 환경변수 `CCSWAP_*` → `AASWAP_*`
- `updatecheck`: 릴리스 URL, `UpgradeCommand()`
- 사용자 노출 문자열, README, AGENTS.md

**완료 조건**: 기존 테스트 전량 통과. 이 단계는 동작을 바꾸지 않는다.

### 1b. 스키마 v2 — provider 스코프 + 이름 주소화
현재(v1):
```json
{"activeAccountNumber":2,"sequence":[1,2],
 "accounts":{"1":{"email":"…"},"2":{…}}}
```
목표(v2):
```json
{"schemaVersion":2,
 "providers":{"claude":{"active":"work","order":["work","personal"],
   "accounts":{"work":{"email":"…"},…}}}}
```
- 자격증명 파일 `credentials/.creds-{num}-{email}.enc` → `credentials/{provider}/{name}.enc`
- 키체인 백업 계정명 `account-{num}-{email}` → `{provider}/{name}`
- 이름 생성: alias가 있으면 alias, 없으면 이메일 로컬파트, 충돌 시 접미사
- `move`/`swap` 삭제

**완료 조건**: v1 스토어를 읽어 v2로 올리는 마이그레이션이 테스트로 증명됨.

### 1c. `account adopt`
`import-store`를 "앞선 스토어 인계"로 일반화. ccswap(v1)과 claude-swap 양쪽.

---

## Phase 2 — 명령 재편, 단절 ✅ 완료

`legacyFlags` 21개, `move`/`swap`, `alias`는 Phase 1에서 이미 삭제됐다. 남은 것:

- `account` 그룹: rename · disable · enable · remove · export · import ·
  unclaimed · adopt
- `dir` 그룹: map · unmap · list
- 최상위에 남는 것: switch · status · list · run · auto · tui · config ·
  upgrade · purge · add · add-token (뒤 둘은 Phase 3에서 login으로 합쳐진다)

**완료 조건**: `aaswap --help`가 10개 + 그룹 2개를 보인다. 동작 변경 없음.

## Phase 3 — `login` + 프롬프트 ✅ 완료
`add`/`add-token` 병합, 네 경우 프롬프트, `--capture`/`--wait`/`--token`.

## Phase 4 — provider 이음매 추출 ✅ 완료
구현체 하나뿐인 `Provider` 인터페이스. `session`의 키체인 직접 호출 제거.

## Phase 5 — Codex ✅ 완료

`--provider` / `AASWAP_PROVIDER` 차원, `~/.codex/auth.json` 읽기·쓰기, id_token
JWT에서 신원 추출, provider별 스토어 분리, 그리고 **할당량**.

### 할당량을 API로 조회하지 않는 이유

`https://chatgpt.com/backend-api/api/codex/usage`는 실제로 존재한다. 다만 브라우저급
챌린지 뒤에 있어서 평범한 HTTP 클라이언트로는 403이다. **우회하지 않는다.**

필요가 없다. Codex는 매 턴 응답으로 rate limit을 받고, 그걸 자기 세션 rollout
(`~/.codex/sessions/**/*.jsonl`)의 `payload.rate_limits`에 이미 적어둔다. 그걸 읽으면
요청도, 소모되는 할당량도, 스로틀도 없다.

```
payload.rate_limits.primary   = {used_percent, window_minutes, resets_at}
payload.rate_limits.secondary = 같은 모양, 또는 null
```

윈도 길이(`window_minutes`)로 매핑한다 — 하루 이하면 rolling, 넘으면 weekly.
`primary`/`secondary`는 플랜마다 어느 쪽인지 달라서 위치로 매핑하면 안 된다.

### 그래서 못 하는 것

rollout에는 **어느 계정 것인지가 적혀 있지 않다.** 그래서 측정치는 지금 살아있는
계정에만 귀속시킨다. 놀고 있는 Codex 계정은 "측정 없음"으로 보고하며, 그건 정직한
답이다 — 그 계정이 마지막에 갖고 있던 윈도는 이미 리셋됐을 가능성이 높다.

안전하게 틀린다: 측정 없는 계정은 소진이 아니라 **미상**으로 읽히므로, 소진됐다고
전환 대상에서 빠지지도 않고 여유 있다고 잘못 선택되지도 않는다.

### 미검증

`run`(세션 모드)의 Codex 동작. `CODEX_HOME`이 `CLAUDE_CONFIG_DIR`와 동등해 보이지만
실기로 시험하지 않았다.
