# 테스트 샌드박스

제품 관리 콘솔이 아니다. **빠른 실험·수정용 로컬 봇**이다. 옛 이름 RAG 플레이그라운드는 **이 앱으로 흡수했고 별도 폴더는 없다.** 코드는 `experimental/rag-leaf-parent/`, 인덱스는 `test-sandbox/data/`, 실행기는 두 폴더의 `start.command`다.

지도: [REPO_MAP.md](REPO_MAP.md). 도구 ID 원본: [contracts/sandbox/tool-parity.json](../contracts/sandbox/tool-parity.json).

## 세 층 (섞지 말 것)

| 이름 | 연다 | 데이터 | RAG | 도구 |
|---|---|---|---|---|
| **관리 콘솔 / 제품** | `masamongd` · 8787 | 제품 `--data-root` SQLite | **제품 기억 켜짐** (2026-09-23, 샌드박스 recipe의 Go 이식, [memory-operations.md](memory-operations.md)) | catalog Action. 키 없으면 fixture |
| **테스트 샌드박스** | `test-sandbox/start.command` 또는 `experimental/rag-leaf-parent/start.command` · **8765** | **`test-sandbox/data/`만** (gitignore) | leaf→parent 인덱스 **여기만** 실행 | **같은 Action ID** + 공개 API 스탠드인 |
| **원문** | 읽기 전용 `.private/raw-corpus/` | 수정·커밋 금지 | 인덱스의 입력 | 해당 없음 |

동기화의 뜻: **이름을 같게 두고 한쪽에서 고친 계약이 다른쪽에 깨지지 않게 한다.** 샌드박스는 이제 실험·비교용이다 — 검색 동작을 바꾸려면 여기서 먼저 평가하고, Go(`internal/memory/*`)에 같이 반영한 뒤 parity 테스트로 같은지 확인한다. 샌드박스의 벡터·요약·가공 산출물은 제품 입력이나 캐시로 재사용하지 않는다.

```
원문 (.private, 읽기만)
        │
        ├─ prepare.py ──► test-sandbox/data/   ← 샌드박스 전용 RAG화 산출
        │
        └─ Go 제품 importer ──► masamongd data-root  ← 제품 경로(원문만 입력)
```

인덱스는 `test-sandbox/data/`에만 둔다. 환경 변수 `MASAMONG_SANDBOX_DATA` 또는 `config.json`의 `data_dir`로 경로를 덮어쓸 수 있다. 다른 설정 파일을 쓰려면 `MASAMONG_SANDBOX_CONFIG`(또는 `prepare.py --config`)를 지정한다. time-gap 실험처럼 기본 인덱스를 덮으면 안 되면 `test-sandbox/data-timegap`처럼 별도 디렉터리와 `--data-dir`을 쓴다.

## 실행

원터치: Finder에서 **`test-sandbox/start.command`** 또는 **`experimental/rag-leaf-parent/start.command`**. 저장소 루트 `start-sandbox.command`도 같다. Terminal 창이 뜨고, 준비되면 브라우저가 `http://127.0.0.1:8765` 를 연다. 기본은 인덱스를 다시 만들지 않는다. 인덱스까지 돌리려면 `SANDBOX_PREPARE=1 ./start.command`.

**데이터셋 고르기:** `test-sandbox/config.<이름>.json` 중 `data_dir`에 인덱스가 있는 것이 실행기 메뉴에 뜬다(예: `discord-demo`, `kakao-demo`). 8초 안에 번호를 누르지 않으면 기본 `config.json`. 묻지 않으려면 `SANDBOX_DATASET=kakao-demo ./start.command`(기본은 `default`). 이미 떠 있는 서버는 바꾸지 않으니 Ctrl+C 후 다시 연다. 같은 설정은 `MASAMONG_SANDBOX_CONFIG=<파일>`로도 준다 — 이때 대화·디버그 기록도 그 인덱스 폴더에 쌓인다.

### 응답 속도

한 턴의 시간은 거의 API 대기다(2026-09-23 측정: 플래너 ~2초, 답변 ~3초, 임베딩 0.5~20초로 편차 큼, 로컬 검색 ~30ms, 재순위 ~0.5초). 샌드박스는 결과를 바꾸지 않고 기다림만 줄인다(`config.json` `query_embed`, 코드 `query_embed.py`).

| 키 | 기본 | 뜻 |
|---|---|---|
| `prefetch` | `true` | 플래너가 도는 동안 원문 질의(q0)를 미리 임베딩한다. 검색이 필요 없는 턴이면 임베딩 1회(무시할 비용)가 버려진다 |
| `hedge_after_ms` | `800` | 임베딩이 이 시간 안에 안 오면 같은 요청을 하나 더 보내고 먼저 온 쪽을 쓴다. `0`이면 끔 |
| `cache` / `cache_entries` | `true` / `512` | 같은 질의·모델·instruction의 벡터를 서버 메모리에 둔다(재시작 시 비움). 같은 질문을 다시 물으면 임베딩 0ms |

q1(semantic_query)은 검색 시작과 동시에 요청한다. 화면의 `임베딩`은 남은 대기 시간이고, `검색`에는 q1 대기가 들어 있다. 출처는 `debug_turns`의 `rank_trace.embed_source`(`hit`/`joined`/`miss`)·`embed_hedged`·`q1_embed_ms`, 누적은 `/api/status`의 `query_embed`. 턴 예산·평가 예산은 다른 스레드의 진행 중 예약 때문에만 모자라면 실패하지 않고 정산을 기다린다.

```bash
cd experimental/rag-leaf-parent
cp config.example.json config.json   # rooms를 인덱스가 있는 채널로
python3 prepare.py                   # 산출은 저장소 루트 test-sandbox/data/
python3 app.py                       # http://127.0.0.1:8765
```

`prepare.py`가 돌 때 터미널에는 `leaf 요약 중 120/7048 (1.7%) · 남음 6,928 · 약 28분 · 전체 9.3%`처럼 한 줄이 이어진다. 같은 숫자는 `test-sandbox/data/prepare_progress.json`과 `/api/prepare/progress`로 샌드박스 상단 막대에도 보인다. 인덱스가 없어도 `app.py`는 켜지며, 준비되는 동안 진행률만 표시한다.

Discord 토큰 없음. 원문 본문은 UI에 덤프하지 않는다.

## 테스트할 때 (화면·로그)

화면에서 할 수 있는 것:

- **질문 틀**: 입력창 옆 `질문 틀`(빈 대화에서는 첫 화면)에 인물·없는 내용·시간·반복 틀이 있다. `[괄호]`만 바꿔 보낸다. 특정 데이터셋 이름은 넣지 않았다.
- **답 아래 진단 줄**: 검색 경로(R0/R2), 근거 수·최고 재순위 점수, 인물 연결(✓/✗), 시간 창, 경고(예: 점수가 하한 근처, 플래너 없이 기본 의도), 턴 시간. `진단`을 열면 질의(q0/q1/q2)·후보원·상위 점수(하한 ✓/✗)·단계별 시간 막대가 나온다. `근거`는 답이 본 원문 조각과 점수, `추적`은 서버 기록 요약.
- **판정**: `맞음` 한 번, 또는 `틀림…`에서 이유(사람·시점·지어냄·모른다고 했어야 함·못 찾음·근거 엉뚱·일부만·도구·말투·느림)와 메모. 새로고침해도 판정과 진단이 다시 붙는다.
- **검수 탭**: `.masamong-scratch/` 아래 `*.review.json`(holdout 검수 시트)을 연다. 지금 켠 인덱스와 방까지 맞는 문항만 원문을 보여 주고(노란 줄 = 정답 근거), `1` 좋음 · `2` 고칠 것 · `3` 버림 · `←/→`로 넘긴다. 판정은 시트의 `verdict`·`note`만 바꾼다. `샌드박스에 물어보기`로 그 문항을 바로 보낸다.
- 왼쪽 아래 **테스트 기록**: 이번 서버 실행의 턴·맞음·틀림·경고 수와 **실행 ID**. 숫자는 서버 로그에서 읽는다.

로그(대화 본문 포함, gitignore): 인덱스 폴더의 `debug_turns.jsonl`에 턴마다 `diagnosis`(위 진단과 같은 함수, `turn_diagnosis.py`)·`run`(서버 실행 ID·데이터셋)·판정(`kind: flag`, `tags`)·브라우저 오류(`kind: client_error`)가 쌓인다. 읽을 때는:

```bash
cd experimental/rag-leaf-parent
python3 debug_report.py                 # 가장 최근 서버 실행의 턴 전부 (Markdown)
python3 debug_report.py --only bad      # 틀림 표시한 턴만
python3 debug_report.py --only warn     # 경고가 붙은 턴만
python3 debug_report.py --run all --since 2h --format json
python3 debug_report.py --id <debug_id> --full
```

경고 기준(`near_floor` 여유 등)은 `config.json`의 `diagnosis` 블록으로 바꾼다(기본 `floor_margin` 0.05). 보고서는 표준 출력으로만 나오고, 커밋하거나 공개 문서에 붙이지 않는다.

## 세션 대화 (테스트 샌드박스만)

샌드박스에서 지금 나눈 말은 Discord 인덱스에 다시 넣지 않는다. `test-sandbox/data/conversations.json`에 채널별 스레드로 남는다.

- **새 대화**: 왼쪽 목록에서 흐름을 끊는다. 예전 창은 목록에서 다시 연다.
- **기억**: 최근 턴은 원문, 길거나 턴이 많으면 앞부분을 LLM 요약(`<세션>`)으로 접는다. 설정 키 `conversation_keep_turns`, `conversation_summarize_chars`.
- 평가·CLI `chat(..., persist=False)`는 이 파일을 건드리지 않는다.
- 요약은 메시지가 `conversation_keep_turns`를 넘을 때 절반까지만 접는다(턴마다 요약하지 않음).

## 모델 교체 (config.json `models`)

모든 모델은 역할별로 바꿀 수 있다. 키는 `.private/eval/secrets.env`에만 두고, 모델 ID는 설정에 둔다.

```json
"models": {
  "llm": "z-ai/glm-5.3-flash",      // 답변
  "planner": "",                    // 도구·의도 분석. 비우면 llm
  "summary": "",                    // leaf 요약·세션 요약. 비우면 llm
  "embed": "qwen/qwen3-embedding-8b",
  "rerank": "voyageai/rerank-2.5-lite"   // R2. 옛 키 rerank_model 도 읽음
}
```

우선순위: `models.<역할>` > `secrets.env EVAL_<ROLE>_MODEL` > 기본값. 랩 화면 설정 patch(`/api/lab/config`)로도 바꿀 수 있다.

조건(검증에 걸리면 `PlaygroundError`):
- OpenRouter `provider/model` 형식.
- `embed` 이름에 `embed`, `rerank` 이름에 `rerank`가 있어야 하고, 채팅 역할에 임베딩·리랭크 모델을 넣을 수 없다.
- **임베딩 모델을 바꾸면** 인덱스를 만든 모델과 달라지므로 `prepare.py --force` 전까지 `load_index`가 거부한다(차원이 같아도 유사도가 무의미하기 때문).
- Qwen3-Embedding 질의 벡터에는 `embed_query_instruction`을 붙인다 (`Instruct: …\nQuery:`). 문서(leaf) 임베딩은 그대로다.
- `rerank_min_score`는 숫자 또는 `{ "voyageai/rerank-2.5-lite": 0.5, "_default": 0.5 }` 맵. R0/R1은 `hybrid_min_cos`(기본 0=꺼짐)로 같은 abstain 상태를 낸다.
- 요청당·턴당·세션당 비용 상한(`max_request_cost_usd`, `turn_budget_usd`, `session_budget_usd`)은 모델과 무관하게 적용된다.

검색 버전 R2의 내부 이름은 `rerank`(벤더 중립)다. 옛 이름 `cohere`는 별칭으로 계속 인식한다.

## 검색 게이트

- 검색 버전 기본값은 **`auto`** (v2): R0 v2 후보 풀(어휘+벡터+요약+인물+별칭+multi-query)을 만들고 confidence를 평가한 뒤 R0로 끝내거나(`r0_confident`) R2를 부르거나(`rerank_uncertain`) 복구 1회 후 R2(`recovery_rerank`)로 간다. 리랭크 불가·실패면 R0 — **R1로 가지 않는다**(결정표: [rag/retrieval-v2.md](rag/retrieval-v2.md)). UI 드롭다운으로 고정 모드를 강제할 수 있다. 어느 모드가 왜 선택됐는지는 `debug_turns`의 `ranking`(요청)·`ranking_applied`·`ranking_auto_reason`·`auto_confidence`·`recovery_performed`·`retrievers_used`.
- 후보원·auto 임계값·복구·rerank 입력 형식은 `config.json` `retrieval` 블록([retrieval-v2 §9](rag/retrieval-v2.md)).

- 플래너 출력 `memory_requirement`: `none`(잡담·도구만) → 채널 기억 검색 0회, `required` → 검색, `optional` → 플래너가 `memory.search`를 넣었을 때만.
- 검색 상태 `rag_state`: `not_attempted` · `candidates` · `empty_in_allowed_scope` · `insufficient_authorized_candidates`(R2 점수가 전부 `rerank_min_score` 미만). 제품 검색 상태와 같은 이름을 쓴다.
- 플래너가 `memory.search.parameters.query`를 주면 **q0를 바꾸지 않고** 추가 후보 질의(semantic_query가 없을 때 q1)로 쓴다. 원문은 `debug_turns.search_query`, rewrite는 `planner_retrieval_query`.
- **의도는 플래너가 정한다** (`time_scope`·`question_kind`·`person_targets`·`sensitive`). 플래너 출력이 없을 때 기본(`intent_fallback: none`)은 중립 의도다. 옛 `최근|요즘|소개|정치…` regex와 `person_aliases` 질의 매칭은 `intent_fallback: regex`로만 켠다(2026-09-23 일반화 감사에서 효과 없음 확인). 인물 별칭은 기본적으로 Discord 계정 ID가 쓴 표시명에서 자동으로 만든다(`retrieval.person.identity: observed`); 수동 표는 `config`/`both`일 때만. 어느 쪽이 쓰였는지는 trace `intent.source`(`planner`/`regex`). 채널·코퍼스별 키워드를 코드에 넣지 않는다 — `identity_terms` 기본값도 비어 있다.

## 제품과 같은 것 (parity 테스트로 고정)

| 축 | 원본 | 샌드박스 | 제품 | 테스트 |
|---|---|---|---|---|
| Action ID·`!` 별칭 | `contracts/sandbox/tool-parity.json` `agent_tools` | `tool_parity.py` | `internal/catalog`, `connectors/discord/commands.go` | 양방향: `catalog/sandbox_parity_test`, `discord/commands_parity_test`(`!별칭` ⊆ Discord) |
| 검색 버전·`auto` v2 cascade·confidence 규칙·retrieval state | `ranking` | `tool_parity.decide_ranking`, `candidates_assess` | `internal/memory/retrieval` (`AutoPolicy.Decide`, `Assess`) | `policy_test`, `test_engine_retrieval.AutoRankingTests` (계약 `cases` 양쪽 검증) |
| 플래너 출력 JSON | `planner_output` | `bot_plan.parse_plan` | `internal/agent.ParsePlannerOutput` | `planner_output_test`, `test_bot_plan.ContractParityTests` |
| 모델 역할 | `models.roles` | `config.json models` | `config.yaml models.<role>` (`models select --role`) | `config/model_roles_parity_test`, `test_models` |
| 채널 응답 정책 | `config.RespondModes` | `bot_runtime.json channels[].respond` | `channels configure --respond` | `test_bot_runtime.RespondPolicyTests` |
| 도구 입력 키 | Action input | `weather.region` `finance.symbol` `fx.base` `search.query` `polls.question/options/duration_hours/allow_multiselect` `images.prompt` | 동일 | — |

CLI와 GUI는 같은 카탈로그를 노출한다: 모든 카탈로그 Action은 CLI 이름으로 실행되고(`cli/catalog_parity_test`), GUI 라우트는 실제 페이지여야 한다(`web/catalog_parity_test`).

## 도구 동기화

자연어 계획과 `!` 명령 모두 제품 `internal/catalog` ID를 쓴다. Discord 별칭은 `internal/connectors/discord/commands.go`와 같다.

| Action | `!` (예) | 샌드박스 구현 | 제품 구현 |
|---|---|---|---|
| `memory.search` | (채팅 기본 RAG) | 로컬 leaf→parent | **실행 가능** (Go 이식) |
| `weather.query` | `!날씨 전주` | Open-Meteo | 기상청 |
| `disasters.query` | `!특보` | KMA 키 없으면 unconfigured | 기상청 |
| `finance.quote` | `!시세 AAPL` | Yahoo chart | Finnhub |
| `finance.fx` | `!환율` | open.er-api.com | ExchangeRate-API |
| `search.web` | `!검색 …` | DuckDuckGo HTML (장소 별칭도 여기) | 카카오 |
| `images.generate` | `!이미지 …` | 유료 호출 안 함 | 설정 시 provider |
| `polls.create` | `!투표 질문 \| 항목1, 항목2` | Discord Poll Create Request JSON (`question.text`, `answers[].poll_media`, `duration`, `allow_multiselect`, `layout_type`) → `test-sandbox/data/sandbox_polls.json` | 제품 SQLite + Discord 네이티브 Poll (봇은 투표 불가) |
| `runtime.doctor` | `!상태` | 인덱스·예산 | daemon doctor |
| `help.version` | `!도움말` | 명령 목록 | help Action |

옛 이름 `local_rag`, `get_weather_forecast`, `web_search` 등은 별칭으로만 받는다. 새 코드·프롬프트는 Action ID를 쓴다.

공급자(기상청 vs Open-Meteo 등)는 키가 다른 로컬 실험이라 다를 수 있다. **실패를 성공인 척 지어내지 않는 계약**과 **ID/`!` 표**가 동기화 대상이다. Go 테스트 `TestSandboxToolParityIDsInCatalog`가 JSON의 ID가 catalog에 있는지 본다.

## 검색 버전 (RAG 동기화)

샌드박스 UI와 `tools/evaluation/rag_upgrade`가 같은 이름이다.

- R0 `hybrid` — 어휘+벡터
- R1 `heuristic` — R0 + 최근·인물 (**ablation 전용**, auto 미사용)
- R2 `rerank`(옛 `cohere`) — R0 풀 + 의미 재순위. **R1을 포함하지 않음.** 기본 `voyageai/rerank-2.5-lite`
- `auto` — R0 v2 후보 관찰 → finish / R2 / recovery→R2

제품 retrieval도 이 이름을 쓴다. 샌드박스 Hit 수치가 제품 gate가 아니다.

## 고칠 때

| 바꿀 것 | 어디 |
|---|---|
| 도구 ID·`!` 별칭 | `contracts/sandbox/tool-parity.json` → Discord `Commands()`와 Python `tool_parity.py`가 맞는지 확인 → `go test ./internal/catalog/` · `python3 -m unittest experimental/rag-leaf-parent/test_tool_parity.py` |
| leaf→parent / 재순위 | `experimental/rag-leaf-parent/engine.py` + `tools/evaluation/rag_upgrade/` |
| 관리 콘솔 문구·Action 실행 | `internal/transport/web`, `internal/application` — 샌드박스 HTML을 제품에 이식하지 않음 |

코드는 git이 추적하는 `experimental/rag-leaf-parent/`만 고친다.
