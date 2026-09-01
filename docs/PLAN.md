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

## Phase 2 — 명령 재편, 단절 (위험: 낮음)

`legacyFlags` 21개, `move`/`swap`, `alias`는 Phase 1에서 이미 삭제됐다. 남은 것:

- `account` 그룹: rename · disable · enable · remove · export · import ·
  unclaimed · adopt
- `dir` 그룹: map · unmap · list
- 최상위에 남는 것: switch · status · list · run · auto · tui · config ·
  upgrade · purge · add · add-token (뒤 둘은 Phase 3에서 login으로 합쳐진다)

**완료 조건**: `aaswap --help`가 10개 + 그룹 2개를 보인다. 동작 변경 없음.

## Phase 3 — `login` + 프롬프트 (위험: 낮음)
`add`/`add-token` 병합, 네 경우 프롬프트, `--capture`/`--wait`/`--token`.

## Phase 4 — provider 이음매 추출 (위험: 중간)
구현체 하나뿐인 `Provider` 인터페이스. `session`의 키체인 직접 호출 제거.

## Phase 5 — Codex (위험: 중간)
`~/.codex/auth.json`, JWT 신원, 자체 할당량.

---

## 계획 검토 — 나온 위험과 대응

1. **1a가 온디스크 상수를 바꾸는 순간 기존 스토어가 안 보인다.** 1a~1c는 커밋이지
   릴리스가 아니므로 1c 전에는 배포하지 않는다. 개발 중 빌드한 바이너리를 실제
   `$HOME`에 대고 실행하지 않는다(테스트 가드는 테스트만 보호한다).
2. **이름 생성이 손실적이다.** `work@a.com`과 `work@b.com`이 둘 다 `work`가 된다.
   같은 이메일이 조직 둘에 있는 경우도 있다. → 로컬파트 우선, 충돌 시 슬롯 순서
   기준 결정적 접미사. **두 번 돌려도 같은 이름이 나와야 한다.**
3. **`..`가 이름이 될 수 있다.** `NormalizeAlias`는 `.`을 허용하므로 `.`과 `..`이
   통과한다. 이름이 경로 성분이 되는 이상 **경로 탈출 가드가 필수**다.
4. **마이그레이션은 멱등·크래시 안전해야 한다.** 순서: (1) 새 자격증명 사본 쓰기
   → (2) v2 로스터 원자적 쓰기 → (3) 옛 사본 삭제. (2) 전에 죽으면 v1이 온전하고,
   (2) 후에 죽으면 v2가 온전하다.
5. **키체인 아이템은 이동이 아니라 복사.** claude-swap 인계와 같은 논리 — 되돌릴 수
   있어야 한다.
6. `activeAccountNumber`는 `*int`, v2의 `active`는 이름 문자열. 매핑 필요.
7. **`AdoptLegacyMarker`는 `.ccswap-` → `.aaswap-`만 인계한다.** `.cswap-`(Python
   시절)은 단절 원칙대로 버린다.
8. ~~`--json` 계약은 v1 유지.~~ **정정: 유지가 불가능하다.** 슬롯 번호가 사라지면
   행의 `"number": 2`가 가리킬 것이 없다. `--json`도 v2로 올리고 `number` 자리에
   `name`을 넣는다. 어차피 깨지므로 `keychain_unavailable` 이름 문제도 Phase 4에서
   같이 정리한다.
