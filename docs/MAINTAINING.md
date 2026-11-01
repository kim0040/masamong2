# 유지보수 가이드

마사몽은 2026-10-13에 기능을 마무리했고, 이후 개선은 **업데이트 패치**로 낸다. 이 문서는 패치 작업을 시작하는 사람(또는 코딩 에이전트)이 **처음 읽는 문서**다. 무엇을 하는지보다 **왜 그렇게 정했는지**를 먼저 적는다.

읽는 순서: 이 문서 → [기여 안내](../CONTRIBUTING.md) → [남은 일](BACKLOG.md)·[알려진 문제](KNOWN_ISSUES.md) → 작업 영역의 운영 문서([기억 운영](memory-operations.md), [도구와 언어](tools-and-languages.md), [저장소 지도](REPO_MAP.md)).

---

## 1. 작업 규칙

- **검증은 로컬에서 먼저 한다.**
  - 바뀐 패키지만 `CI=true go test -count=1 -p 1 <패키지>`로 돌리고, 보고에 명령과 결과를 그대로 적는다.
  - Windows 전용 동작은 Windows에서 직접 돌려야 검증된 것으로 본다.
- **저사양 장비를 전제로 한다.**
  - `go test ./...`와 패키지 전체 `-race`는 무겁다. `-race`는 `-run`으로 좁혀서 쓴다.
- **비밀과 개인정보를 출력하지 않는다.**
  - env 값, API 키, 사용자 ID, 대화 본문, 닉네임을 로그·문서·커밋·이슈에 쓰지 않는다. 집계 수치와 문항 ID만 쓴다.
  - 원문과 `.private/`는 수정하지도 커밋하지도 않는다.
  - 테스트에는 가짜 ID(예: `100100000000000101`)와 가상의 이름만 쓴다.
- **근본 원인을 고친다.**
  - 임시방편 하드코딩과 한국어 하드코딩은 하지 않는다. 언어별 내용은 언어팩 `internal/lang/packs`와 `locales/`에 둔다.
  - 새 도구는 `internal/tools` Builtins에, 새 공급자는 `hosts.json`에 넣는다. 콘솔 코드에는 넣지 않는다.
  - 특정 서버·채널 이름에 기대는 기본값을 만들지 않는다. 필요하면 관리자가 켜는 선택지로 둔다(예: 서버 로그의 `auto`).
- **새 제품 기능은 Action과 GUI 흐름까지 만들어야 끝난다**([ADR-16](adr/ADR-16-gui-default-svelte-console.md)).
  - 콘솔을 고치면 `make ui`로 `ui/dist`를 다시 빌드해 함께 커밋한다.
  - 문구는 `ui/scripts/strings.py`(ko·en)와 `locales/ui`(ja·es·zh-Hans)에 넣는다.
- **검색 동작을 바꾸면 샌드박스(`experimental/rag-leaf-parent`)와 Go를 함께 고친다.**
  - parity 테스트로 확인한다.
  - 한국어 플래너·문맥·기억·보안·색인 요약 프롬프트는 샌드박스와 **바이트 단위로 같아야** 한다.
- **문서를 고치면 `make docs`를 돌린다.** 한국어 위키나 README를 고쳤으면 다른 언어도 고친 뒤 `make docs-record`를 돌린다.
- **스크린샷은 `output/layout/`에만** 저장하고 커밋하지 않는다. README 화면만 예외로, 연습 모드에서 찍어 `docs/images/readme/`에 커밋한다.
- **UX 문제는 예시로 받는다.** 한 화면의 문제를 찾으면 같은 종류의 문제를 콘솔 전체에서 찾아 함께 고친다.

---

## 2. 제품 원칙 — 바꾸지 않는 것

- **마사몽은 친구들 Discord 방의 친구 봇**이다. 팩트체크 봇이 아니다.
  - 친구도 기억력이 완벽하지 않다. 처참하지만 않으면 된다.
  - 허세와 추측은 괜찮다. 연애·건강·돈·범죄에 관한 해로운 거짓 소문은 안 된다.
- **기억이 가장 중요한 기능**이다. 카카오톡 기록도 일부 쓴다. 개인별 기억만으로는 부족하고 **채널의 대화 흐름**을 따라가야 한다.
- **기억 품질은 기대한 수준을 지킨다.** 임베딩·검색 성능은 보통 기대하는 수준이 나와야 한다. 메모리를 아끼려고 품질을 깎지 않는다. 극저사양 대응은 극단적인 경우에만 한다.
- **오래 안정적으로 돌아야 한다.** 몇 달, 몇 년을 24시간 돌려도 성능과 유지보수가 지속 가능해야 한다.
- **엄격한 개인정보 기능은 있지만 기본은 꺼짐**이다(관리자가 선택). 친구끼리 쓰는 봇이라 개인정보를 너무 앞세우면 재미가 없다.
- **처음부터 다국어로 만든다.**
  - 신호는 구조(답장, 멘션, 시간, 작성자)를 먼저 본다.
  - 의미는 언어팩 안 프롬프트의 작은 분류 모델이 판단한다.
  - 단어 목록은 보조로만 쓴다.
- **딴 얘기는 격리한다.** 부른 사람과 대화하는 중에 다른 사람들의 얘기를 끌어오지 않는다. 모델에게 "무시하라"고 지시하지 않고, **문맥에 무엇을 넣을지**로 해결한다.
- **먼저 끼어들기는 실험 기능**이다(기본 꺼짐). 음성은 계획에서 뺐다.
- **SQLite만 쓴다.** TiDB 코드는 지우지 않고 꺼 두었다. 여러 백엔드를 다시 제안하지 않는다.
- **관리는 GUI가 기본**이다(Svelte 콘솔).
- **코드 서명과 공증은 보류했다**(비용, 계정 문제).
- 가격·모델·캐시 정보는 자주 바뀌므로 **API에서 먼저 가져온다.** 수동 입력은 선택이고, 내장 표는 마지막 수단이다.

---

## 3. 결정 이력

| 날짜 | 결정 | 이유 |
|---|---|---|
| 09-14~ | 기존 가공 방식과 산출물은 새 제품 입력에서 모두 제외([RAW_DATA_POLICY](RAW_DATA_POLICY.md)) | 원문부터 새 파이프라인으로 처리하려고 |
| 09-21 | 구조화 판단 모델 Jev를 끼어들기 판정에 시범 적용 → **보류** | 한국어 민감 재현율 0.571 등 품질 부족 |
| 09-23 | 샌드박스 RAG를 **Go로 전부 이식**. 샌드박스는 실험·비교 환경으로 남김 | 검증된 레시피를 그대로 쓰고, parity로 둘이 어긋나지 않게 하려고 |
| 09-23 | 과적합 감사 결과 수동 별칭·키워드 정규식을 기본 끔. 기본 leaf는 8개 또는 30분 공백 | holdout에서 효과 0. 범용성 우선 |
| 09-28 | SQLite 전용, 새 서버는 승인 대기, 한국어 RAG 프롬프트는 샌드박스와 동일하게 유지 | 1인 운영 범위로 축소 |
| 10-01~02 | 콘솔 재구성(설정마다 주인 페이지 하나), 실사용 시나리오 52개, 업데이트·초기화 | 관리 UX 자동화 |
| 10-11 | Hermes·OpenClaw에서 차용(대화 문맥, 도구 루프, 사람·채널 메모, 스레드, 끼어들기 실험). 설계와 근거는 [기억·대화·도구 설계](plan-memory-conversation.md) | 마사몽에 맞는 것만 가져오려고 |
| 10-12 | R0(재순위 없는 공급자)의 관련성 하한은 0으로 유지 | 경계값이 방 크기와 임베딩 모델마다 달라지고, 안전한 값은 효과가 작음 |
| 10-12 | **메시지 단위 벡터를 기본으로 켬**(`rrf_weight` 0.1). 결과 단위는 조각(leaf) 그대로 두고 메시지 벡터는 후보 찾기 보조로만 씀 | 8줄을 벡터 하나로 묶을 때 생기는 희석 때문에 놓치던 것을 보완. 근거는 [기억 운영 §15](memory-operations.md) |
| 10-12 | 저사양(1코어·512MB) 보강과 기기 성능 단계 | 라즈베리파이급에서도 멈추지 않게 하려고([기억 운영 §16](memory-operations.md)) |
| 10-13 | **기본 임베딩 폭 1024.** 재순위가 없는 구성만 추천에서 전체 폭을 제안 | 재순위가 있으면 품질 차이가 잡음 수준이고 힙은 21~27% 줄어서([측정 기록](rag/embedding-width-2026-10.md)) |
| 10-13 | 기억 엔진 호출(임베딩·재순위·요약)을 호출 장부에 기록. parity는 차이 0만 통과(ε 동점 규칙) | 턴 상세의 비용 출처를 보이고, 플랫폼마다 다르던 검사를 결정적으로 만들려고 |
| 10-13 | **메시지 벡터 기본 폭 512 적용, ADR-18은 0단계부터** 진행 | MRL 모델은 leaf 폭이 512 이하면 그 폭, 아니면 512를 쓴다. MRL 목록 밖 모델은 자르지 않고 전체 폭을 쓴다. 512는 품질 차이가 잡음 수준이고 힙이 15~19% 준다. ADR-18은 측정·게이트를 먼저 본다 |
| 10-13 | **DM 대화 지원**(LIVE-15). 기본은 승인된 서버 멤버·운영자, 1인 하루 30회·전체 하루 100회(`dm`). DM은 서버 기억과 섞지 않음 | 구 마사몽처럼 DM에서도 지원 기능을 쓰게 하고, 비용과 사생활은 관리자가 정하게 하려고 |
| 10-13 | 구 마사몽(Python, [kim0040/masamong](https://github.com/kim0040/masamong)) 지원·업데이트 종료. 이후 개선은 마사몽2에서만 함. 구 저장소는 실제 Discord ID와 로그 파일을 지우도록 기록을 다시 씀 | 두 저장소를 같이 유지할 여력이 없고, 공개 저장소에 남은 개인 정보를 치우려고 |

---

## 4. 업데이트 패치 내기

패치를 실제 설치본에 전달하는 방식은 아직 정하지 않았다([남은 일](BACKLOG.md)의 "배포·업데이트 경로").

**지금 상태**
- 버전 문자열은 `internal/application/service.go`의 `Version`이고, 지금 값은 `0.2.0-dev`다.
  - `-dev` 빌드는 업데이트 확인을 하지 않는다.
  - 릴리스 빌드는 `scripts/release.sh`가 `-X`로 버전을 박는다.
- 태그도 GitHub Release도 아직 없다.
- 콘솔의 업데이트 확인과 **지금 업데이트**, 그리고 설치 스크립트(`install.sh`, `install.ps1`)는 모두 `github.com/kim0040/masamong2`의 **공개** Release에서 내려받는다.
  - 첫 Release를 내기 전까지는 이 경로가 404를 받는다. 콘솔은 "확인할 수 없음"으로 표시하고, 설치 스크립트는 소스 빌드를 안내한다.
- `release` workflow(태그 푸시)는 GitHub Actions가 필요하다.

**Actions 없이 로컬에서 릴리스하기** (macOS)
```sh
scripts/release.sh v1.0.0            # build/release/에 모든 대상 아카이브와 SHA256SUMS 생성
scripts/verify-release.sh build/release
git tag v1.0.0 && git push origin v1.0.0     # [주의] Actions가 켜져 있으면 release workflow도 돈다
gh release create v1.0.0 build/release/* --title "Masamong v1.0.0" --generate-notes
```
- 아카이브 이름과 `SHA256SUMS`는 앱 내 업데이트(`internal/application/selfupdate.go`)와 맺은 계약이다. 손으로 이름을 바꾸지 않는다.

**패치를 낼 때 확인할 것**
1. 기본값을 바꾸면 기존 설치에서 무엇이 일어나는지 적는다. 예를 들어 임베딩 폭이나 메시지 벡터 폭을 바꾸면 다시 임베딩해야 한다. 그 비용과 시간을 [기억 운영](memory-operations.md)과 릴리스 노트에 적는다.
2. 마이그레이션은 이전 버전으로 롤백해도 안전해야 한다. 저사양 기기에서 시작을 막거나 디스크를 크게 쓰는 마이그레이션은 피한다([기억 운영 §16](memory-operations.md)).
3. 바뀐 영역의 로컬 테스트를 돌린다. 검색을 바꿨으면 parity도 돌린다(§5).
4. 위키 [릴리스 체크리스트](wiki/Release-Checklist.md)를 따른다.

---

## 5. 검증 명령

```sh
# Go: 바뀐 패키지만 순서대로
go build ./... && go vet ./internal/chat ./internal/memory/...
CI=true go test -count=1 -p 1 ./internal/chat ./internal/memory/... ./internal/transport/web ./locales
CI=true go test -count=1 -p 1 -timeout 60m ./internal/application         # 장비에 따라 3분~15분
go test -race -count=1 -run 'TestName1|TestName2' ./internal/application   # -race는 좁혀서

# 콘솔
make ui                                                      # ui/dist 재빌드(커밋)
(cd internal/transport/web/ui && npx svelte-check --fail-on-warnings && node --test scripts/console-logic.test.mjs)

# 문서
make docs

# 샌드박스 단위 테스트(numpy 필요). 아래 명령은 모두 저장소 루트에서 실행한다
(cd experimental/rag-leaf-parent && PYTHONPATH=../../tools/evaluation uv run --no-project --with numpy python -m unittest discover -s .)

# 실데이터 검색 parity(비공개 샌드박스 인덱스 test-sandbox/ 필요, API 호출 없음)
O="$PWD/.masamong-scratch/parity" && mkdir -p "$O"
MASAMONG_SANDBOX_CONFIG="$PWD/test-sandbox/config.discord-gap30.json" PARITY_N=80 \
  python3 experimental/rag-leaf-parent/parity_dump.py "$O" discord        # numpy 있는 python
MASAMONG_SANDBOX_DATA="$PWD/test-sandbox/data-discord-gap30" MASAMONG_SANDBOX_CONFIG="$PWD/test-sandbox/config.discord-gap30.json" \
  MASAMONG_PARITY_RETRIEVE="$O/retrieve_discord.json" go test -count=1 ./internal/memory/search/ -run TestSandboxRetrieveParity -v
#   통과 조건: fusion_diff 0, hits_diff 0 (2026-10-13 Mac·Windows 모두 통과)

# 실사용 시나리오(실제 API, 약 $0.3~0.5)
set -a; . ./.env; set +a
go test -c -o /tmp/app.test ./internal/application
(cd internal/application && MASAMONG_LIVE_KEYS_DIR="$PWD/../../.private/test-keys" MASAMONG_TOOL_MODE=loop \
  MASAMONG_SCENARIO_FILE=testdata/tools_regression_ko.json MASAMONG_SCENARIO_REPORT=/tmp/ko.json \
  /tmp/app.test -test.run 'TestLiveScenarios$' -test.v -test.timeout 40m)
#   세트: testdata/scenarios_ko.json(52), tools_regression_ko.json(59), tools_regression_en.json(59)
```

**기억 품질 측정**(실제 기록, 비공개 데이터 필요)은 `tools/evaluation/memory_dims/`(러너와 `run_sweep.sh`, `agg.py`)로 한다.
- 데이터 루트 사본은 `.masamong-scratch/`에만 둔다.
- 평가 루트는 설정을 DB에 저장한다. 그래서 `config.yaml`을 고쳐도 반영되지 않는다. 러너의 `config.patch` 경로를 쓴다.
- 지금까지의 수치는 합성·미검수 문항(세트당 40+10, 신뢰구간 ±13~15%p)이다. 사람이 검수한 gold는 아직 없다.

---

## 6. 개발 장비 준비

1. 저장소를 clone하고 main에서 시작한다. 쓰던 버전은 Go 1.27.x, Node 26.x, uv 0.13.x다.
2. **에이전트 지침(선택, git 제외)**: 코딩 에이전트에게 개인 작업 규칙을 주려면 저장소 루트에 `AGENTS.md`를 만든다. `.gitignore`에 들어 있어 커밋되지 않는다.
3. **키(git 제외)**: 실제 API를 부르는 테스트에만 필요하다. `.env`(`.env.example` 참고)에 테스트용 키를 넣고, 실사용 시나리오용 키 폴더는 이렇게 만든다.
   ```sh
   umask 077; mkdir -p .private/test-keys
   set -a; . ./.env; set +a
   for p in openrouter:OPENROUTER_API_KEY kakao:KAKAO_REST_API_KEY kma:KMA_API_KEY \
            finnhub:FINNHUB_API_KEY exchangerate:EXCHANGERATE_API_KEY linkup:LINKUP_API_KEY; do
     v=$(printenv ${p#*:}) && [ -n "$v" ] && printf '%s' "$v" > .private/test-keys/${p%%:*}; done
   ```
4. **비공개 데이터(git 제외)**: 실제 대화로 품질을 재려면 직접 가진 기록을 쓴다. 저장소에는 들어 있지 않다.

   | 경로 | 용도 | 필요한 때 |
   |---|---|---|
   | `.private/raw-corpus/` | 원문 코퍼스 | 실데이터 적재와 gold 생성 |
   | `test-sandbox/` | 샌드박스 인덱스 | 실데이터 parity 테스트 |
   | `.masamong-scratch/` | 평가 gold, 데이터 루트, 보고서 | 측정을 이어서 할 때 |

5. `make docs`와 `CI=true go test -count=1 -p 1 ./internal/chat ./internal/memory/...`가 통과하면 준비가 끝난 것이다.
6. **Windows에서는**:
   - Git Bash에서 `python`을 쓴다(`python3`은 Microsoft Store 스텁일 수 있다).
   - `-race`에는 cgo용 gcc(WinLibs)가 필요하다.
   - `.env`가 CRLF면 `set -a; source <(tr -d '\r' < .env); set +a`로 읽는다.
   - PowerShell에서 `bash`를 치면 Git Bash가 아니라 WSL 실행기(`WindowsApps\bash.exe`)가 실행될 수 있다. Git Bash는 전체 경로로 부른다.
   - **운영 인스턴스가 같은 장비에 있으면** 개발용과 데이터 루트·포트·Discord 토큰을 나누고, 운영 실행 파일로는 읽기 명령만 쓴다.

---

## 7. 공급자 호출 제한 (2026-10 조사, 병렬화 기준)

| 서비스 | 제한 | 병렬 상한(권장) | 비고 |
|---|---|---|---|
| OpenRouter | 유료 모델 RPM 명시 없음, `:free`만 20 RPM | 제한 없음(색인 기본 8) | 402·403은 예산 문제라 재시도하지 않음. 429는 Retry-After를 따름 |
| OpenAI | 등급별 수천 RPM | 여유 있음 | `slow_down` 429가 일찍 올 수 있음 |
| Gemini(OpenAI 호환) | 모델별 값은 AI Studio에만 있음 | 보통 | 429 RESOURCE_EXHAUSTED |
| NanoGPT | 초당 25 | 여유 있음 | 키별 일일 상한 옵션 |
| CometAPI | 공개 안 됨 | 보수적으로 시작 | 업스트림을 요청마다 바꿔 보내는 듯함 |
| 카카오 로컬 | 하루 10만 | 2 | 초과는 **HTTP 400**(-10/-11)이고 429가 아님 |
| 기상청(data.go.kr) | 초당 제한, 개발 계정 하루 1만 | 1 | |
| Finnhub 무료 | 분당 약 60 | 1 | KRX(.KS) 없음 |
| ExchangeRate-API | 월 1,500 | 1 | 하루 1회 갱신 |
| Linkup | 조직당 초당 10, 크레딧제 | 2 | |
| Jina Reader | 키 없이 20 RPM, 키 있으면 500 | 1 / 4 | |
| Open-Meteo | 분당 600, 하루 1만 | 2 | |

---

## 8. 함정

- **워크트리**는 오래된 기준에서 시작할 수 있다. WIP를 확인·보존한 뒤 깨끗한 작업 트리에서 `git merge --ff-only <기준 SHA>`로 맞춘다. 워크트리를 만들 때는 저장소 루트에서 실행한다.
- **세션 임시 폴더**는 세션이 끝나면 사라진다. 남길 것은 `.masamong-scratch/`나 `.private/`에 둔다(둘 다 git 제외).
- **한국어 골든**(`internal/chat/testdata`, `internal/memory/search/testdata`)은 바이트 단위로 비교한다. 프롬프트를 고치면 샌드박스도 고치고 골든을 다시 만든다.
- **macOS는 파일 이름의 대소문자를 구분하지 않는다.** 런처 이름 때문에 `Masamong`이 `masamong` 바이너리를 덮어쓴 적이 있다.
- **집계에도 ID가 섞일 수 있다.** 로그를 분류별로 셀 때 분류 이름에 ID가 들어가는 경우가 있다. 출력하기 전에 숫자를 가린다. 모델이 쓴 사유 문구(예: 끼어들기 판단 이유)에는 사람 이름이 들어갈 수 있으니 출력하지 않는다.
- **실사용 시나리오가 갑자기 많이 실패하면** 보고서 JSON의 `tool_loop.stop`과 `fallback`부터 본다.
- **Windows에서 `ui/dist`가 걸린 rebase 충돌**은 손으로 고치지 않는다.
  - `npm ci && npm run build`를 돌린 뒤 `git add -A internal/transport/web/ui/dist`로 넣는다.
  - `docs/translations.json` 충돌은 한쪽을 고른 뒤 `go run ./tools/doccheck -write`를 돌린다.
- **parity 덤프 경로**: `parity_dump.py`는 설정 경로를 `experimental/rag-leaf-parent` 기준으로 풀 수 있다. 설정 경로는 절대 경로로 넘긴다.
