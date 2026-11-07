# 도구 설정과 언어

2026-09-23. 마사몽이 쓰는 도구(날씨·시세·환율·검색·투표·기억 등)를 **켜고 끄고, 채널마다 다르게 하고, 사용량을 제한하고, 코드 없이 새 도구를 붙이는** 방법, 그리고 한국어 외 언어로 돌리는 방법이다. 기억(RAG) 운영은 [memory-operations.md](memory-operations.md).

## 1. 구조 한눈에

| 층 | 코드 | 하는 일 |
|---|---|---|
| 도구 목록 | `internal/tools` | 내장 도구 정의(어떤 Action이 도구인지, 기본으로 대화·명령에 쓰는지, 필수 파라미터), config `tools:` 검증, 사용량 카운터(`tool_usage` 테이블) |
| 관문 | `internal/application/tools.go` | 모든 도구 호출이 지나는 한 곳. 켜짐 → 기능 스위치(`features`) → 채널 스위치 → 호출 경로(대화/명령) → 사용량 한도 순으로 검사하고 기본 파라미터를 채운다 |
| 대화 | `internal/chat` | 플래너 메뉴·필수 파라미터를 위 목록에서 받는다(코드에 도구 목록이 없다) |
| 언어팩 | `internal/lang/packs/*.json` | 프롬프트·기억 규칙·근거 표시·요약 프롬프트·질의 지시문·조사/대명사/불용어 |
| 화면 문구 | `locales/*.json` | Discord 명령 이름·설명·기본 `!`단어, 결과 카드 문구, 도구 거절 메시지 |

**누가 부르든 같은 관문을 지난다.** 대화 플래너, Discord `!명령`·슬래시 명령, CLI, 관리 GUI 모두 `Service.Execute`로 들어오므로 도구를 끄면 어디서도 안 돌고, 한도도 모든 경로에 똑같이 적용된다. 호출자 정보(`Invoker`: 경로·서버·채널·사용자)는 프로세스 안에서만 붙고 HTTP/JSON으로는 들어올 수 없다.

## 2. 지금 있는 도구

`masamong tools list`로 실제 상태를 본다(켜짐·대화·명령·구현 여부·오늘 사용량·한도).

| 도구 | 대화 기본 | 명령 | 비고 |
|---|---|---|---|
| `memory.search` | 켜짐 | — | 채널 기억. `memory.retrieve`와 채널 `memory_use`도 따른다 |
| `weather.query` | 켜짐 | `!날씨` `/weather` | 키 없이 Open-Meteo로 동작, 기상청 키가 있으면 국내는 기상청. 기본 지역은 `defaults.region`으로 바꾼다 |
| `disasters.query` | 켜짐 | `!특보` `/alerts` | 기상청 키 필요. 묻지 않아도 알리는 **재난 알림**은 따로 있다(아래) |
| `finance.quote` | 켜짐 | `!시세` `/quote` | Finnhub 키. Finnhub 요금제에 없는 종목(한국 상장 `005930.KS`, 지수, 한글 이름)이나 키가 없을 때는 인터넷 검색으로 답하고 `source: search`로 표시한다(구 봇도 한국 시장은 검색으로 답하는 쪽으로 정리했다) |
| `finance.fx` | 켜짐 | `!환율` `/fx` | 키 없이도 공개 엔드포인트로 동작 |
| `search.web` | 켜짐 | `!검색` `/search` | 인터넷 검색. Tavily·Linkup·Exa·Serper·Brave·TinyFish·카카오 중 키가 있는 것을 `web_search.order` 순서대로(실패하면 다음). `depth`(quick·standard·deep)와 `topic`(news)을 서비스별 옵션으로 옮긴다(표는 `websearch.go` 머리말). 한 서비스가 깊이별 제한 시간(quick 8초·standard 10초·deep 15초, 실측 근거는 `features.go`의 `searchDeadline`) 안에 답하지 않으면 `passed_over`에 `timeout`으로 적고 다음 서비스로 넘어간다. `internal/providers/websearch` |
| `web.read` | **꺼짐**(직접 켬) | — | 메시지 속 링크 본문 읽기(Jina Reader, 키 없이 동작). 링크가 외부 서비스로 가므로 `Builtin.OptIn`: 운영자가 켜기 전까지 꺼져 있고 콘솔은 켤 때 무엇이 나가는지 확인한다. 내부망 주소는 거부 |
| `places.search` | 켜짐 | `/places` | 맛집·카페·가게 찾기(카카오맵 키워드 검색, 웹 검색과 같은 카카오 REST 키). "분위기 좋은"처럼 카카오가 못 맞추는 말이 섞여 결과가 없으면 각 단어 + 가게 종류("홍대 카페")로 최대 2번 더 찾고(카카오가 지역을 읽은 결과만) `searched_as`로 알린다. 지역 없이 "근처 맛집"이면 카카오가 전국 유명 가게를 돌려주므로 버리고 `no_area`로 어디인지 되묻게 한다. 검색어의 한 단어가 결과 **대부분**의 이름·주소에 있으면("스타벅스 리저브", "성수동 카페") 특정 장소로 보고 통과 — 한두 곳 이름에 우연히 든 말("동네 카페")로는 통과하지 않는다 |
| `activity.ranking` | 켜짐 | `/ranking` | 채널 수다왕(오늘·이번 주·이번 달·전체). 메시지 **수만** 날짜·서버·채널·사람별로 세고(`activity_counts`) 본문은 저장하지 않는다. 기억 동의를 철회한 사람·봇·승인 안 된 서버는 세지 않고, 보관 기간은 `retention.turn_days`를 따른다 |
| `polls.create` | 켜짐 | `!투표` `/poll` | 대화에서 만들면 Discord 투표로 실제 올라간다. `chat` CLI(dry-run)에서는 만들지 않는다 |
| `help.version` | 켜짐 | `!도움말` `/help` | 지금 쓸 수 있는 도구 목록을 돌려준다("뭐 할 수 있어?") |
| `images.generate` | 켜짐 | `/image` | 대화("고양이 그려줘")와 명령 모두. 연결 → 모델에서 `image` 역할 모델을 골라야 그린다(없으면 `unconfigured`로 안내). 대화에서 그린 그림은 `/image`와 같은 카드로 올라간다. **대화에서는 백그라운드로 그린다**(기본, `tools.images.generate.defaults.async: false`면 예전처럼 그림이 나올 때까지 답을 기다린다): 확인(빈 요청·금지어·모델·예산·비율)을 마치면 작업을 줄에 넣고 곧바로 `queued`를 돌려주며, 답은 언어팩 `chat.tool_queued`("그리는 중이야, 금방 올릴게" 한 줄, 그림을 다 그린 듯 묘사하지 않기)를 따른다. 그림은 하나씩 그려 부탁한 메시지에 답글로 올라가고(멘션 없음, 메시지가 지워졌으면 답글 없이), 실패하면 짧은 안내 한 줄(`image.failed`)이 올라간다. 줄은 기다리는 작업 3개까지, 한 사람은 한 번에 하나(`image.queue_mine`, 줄이 가득 찬 경우는 `image.queue_full`), 같은 사람의 같은 요청(대소문자·공백·끝 문장부호 무시, 비율이 다르면 다른 요청)은 10분 안에 같은 작업을 돌려준다(`already_queued`). 이미 줄에 있는 사람의 반복 요청과 가득 찬 줄의 거절은 도구 사용 한도(일일 횟수)에 세지 않는다. 그리기 전과 올리기 전에 채널이 일시정지·꺼짐이거나 서버가 승인 안 됐거나 도구가 꺼졌거나 요청자가 차단·기억 동의 철회를 했거나 **요청 메시지가 지워졌으면** 말없이 버린다(지워진 요청의 그림은 답글 없이도 올리지 않는다). 게이트웨이가 재연결 중이거나 서버 목록이 잠깐 비어 있는 것은 버릴 이유가 아니다: 최대 30초 기다린 뒤에도 안 되면 `image.failed`를 올린다. 봇이 그 채널에 파일을 첨부할 수 없으면 그리지 않고(비용 없음) 운영자에게 권한 경고를 남기며, 올리기가 거절·실패하면(10 MiB 초과, 파일 없음, 재시도 소진) `image.failed`를 올린다(권한 부족은 경고만). 데몬이 멈추면 기다리던 작업도 버린다(저장하지 않는다). `/image` 명령과 콘솔 시험은 항상 기다린다 |
| `fortune.*` | 켜짐 | `/fortune` | 대화 모델로 쓴다. 생일 정보는 운세 동의가 있을 때만 쓴다 |
| `notices.*`, `transfer.recent` | 켜짐 | `/notices` `/transfer` | 내장 학교 게시판 목록(또는 데이터 폴더 `notices/catalog.json`) |
| `polls.vote/close/result` | — | 투표 버튼 | 투표 기능 내부 |
| `chat.respond` | — | — | **대화 답변 자체.** 여기에 한도를 걸면 답변 횟수가 제한된다 |

2026-09-23 확인(실제 모델, 새 인스턴스): 환율 질문 → `finance.fx` 실시간 값 그대로 전달, 투표 요청 → `polls.create`(dry-run), "뭐 할 수 있어?" → `help.version`, 키 없는 주가 → 지어내지 않고 "연결 안 돼 있다"고 답함, 영어 채널 → 영어로 답함. 5턴 약 $0.0025. 날씨·특보·시세·검색의 **실키 호출은 이 환경에 키가 없어 확인하지 못했다**(각 공급자 어댑터 테스트는 가짜 서버로 통과).

### 재난 알림 (지진·특보 자동 공지)

`disasters.query`는 물어볼 때만 답한다. 구 봇처럼 **먼저 알리는** 기능은 스케줄러가 맡는다(`internal/application/disaster_alerts.go`).

- 지진: 1분마다 기상청 `getEqkMsg`. 국내 규모 4.0·해외 7.0 이상(설정), "국내영향없음" 통보 제외. 72시간·150 km 안의 후속 지진은 같은 사건으로 묶어 **처음 올린 메시지를 고친다**(채널이 도배되지 않게).
- 특보: 5분마다 `wrn_now_data.php`. 경보는 육상 전국, 주의보는 `warning_regions`에 든 지역만, 바다(S로 시작하는 구역)는 `marine: true`일 때만. 새로 나온 것과 해제된 것을 종류·단계별로 묶어 한 번에.
- 처음 켤 때는 기준점만 잡고 지난 소식을 한꺼번에 올리지 않는다. 연습 모드·Discord 미연결·기상청 키 없음이면 쉰다(`disasters.alerts`가 이유를 돌려준다).
- 켜고 끄기: 콘솔 **도구 → 특보·지진 → 운영** 탭(전체·특보·바다·규모·지역), 채널마다 **방 → 답장** 탭의 재난 알림 스위치(`channels.configure disaster_alerts=off`). config는 `disaster_alerts:`(`enabled`, `earthquake_min_domestic`, `earthquake_min_overseas`, `warnings`, `warning_regions`, `marine`)와 채널 `disaster_alerts: off`.
- 보내는 곳: 설정된 채널 중 승인된 서버, 답장이 꺼지지 않았고 재난 알림을 끄지 않은 채널.

## 3. 켜고 끄기·한도 — CLI

```bash
M="masamong --data-root <dir>"
$M tools list                                         # 상태·오늘 사용량
$M tools test --id finance.fx                          # 실제로 한 번 실행(한도에 포함)
$M tools test --id finance.quote --input '{"symbol":"NVDA"}'

$M tools configure --id search.web --enabled off       # 어디서도 안 씀
$M tools configure --id polls.create --chat off        # 대화에선 안 쓰고 !투표만
$M tools configure --id weather.query --commands off   # 명령은 막고 대화에서만
```

도구마다 대화에서 쓸 수 있는지, 명령어가 있는지는 정해져 있다(`tools list`의 `can_chat`·`can_command`). 설정으로는 끌 수만 있고, 없는 쪽을 켜려고 하면 거절한다(예: 운세·공지는 동의 절차를 거치는 명령으로만 쓴다. 그림은 구 봇 기록에서 대화 요청이 많아 대화에서도 그리며, 이미지 도구의 하루 한도와 월 예산이 비용을 막는다). 콘솔은 그 칸에 스위치 대신 '해당 없음'을 보여 준다.

```sh
$M tools configure --id weather.query --aliases "날씨,w,weather"   # !단어 바꾸기
$M tools configure --id weather.query --defaults '{"region":"부산"}'
$M tools configure --id search.web --per_day 300 --per_user_per_day 20 --cooldown_sec 10
$M tools configure --id chat.respond --per_user_per_day 200          # 답변 횟수 제한
$M tools configure --id search.web --enabled default   # 설정 지우고 기본값으로

# 채널 하나에서만 끄기/켜기 (그 채널의 대화·명령 모두)
$M tools configure --id search.web --guild_id G --channel_id C --enabled off
$M tools configure --id search.web --guild_id G --channel_id C --enabled default
```

모든 변경은 config 리비전으로 남고(`config diff`/`rollback` 가능), 데몬에 바로 반영된다. 슬래시 명령 목록도 설정이 바뀌면 다시 동기화된다(바뀐 게 없으면 호출 안 함).

**한도 의미** (0 = 제한 없음, 하루 = `i18n.timezone` 기준 달력 날짜)

| 키 | 뜻 |
|---|---|
| `per_day` | 모든 호출 합계(CLI 포함) |
| `per_user_per_day` | Discord 사용자 1명 |
| `per_channel_per_day` | 채널 1개 |
| `cooldown_sec` | 같은 사용자의 연속 호출 간격 |

한도에 걸리면 `TOOL_LIMITED`로 거절하고 사용자 언어로 이유를 알려준다(대화에서는 모델이 친구 말투로 전한다). 답변(`chat.respond`) 한도·월 예산(`limits.monthly_budget_usd`)에 걸리면 Discord에서는 조용히 답하지 않는다. 사용자 ID는 해시로만 저장한다. 공급자별 일일 한도(`providers.<id>.daily_limit`)는 그대로 따로 있다.

**한 턴의 도구는 동시에 돈다.** 도구 루프의 한 라운드에서 모델이 부른 호출들, 플래너 경로의 도구들과 기억 검색은 한 번에 최대 4개(`chat.ToolParallel`)까지 함께 실행한다. 같은 도구 이름의 호출은 모델 순서대로 직렬 실행해 사용량·cooldown의 예약 순서를 지키고, 서로 다른 도구는 병렬 실행한다. 턴 한도(`limits.per_request_budget_usd`, `limits.max_external_attempts`)는 모델·계획의 순서대로 판정한다. 플래너·루프·답변 모델은 시작할 때 호출 수를 `Guard`에 예약하고 완료할 때 비용을 더하므로, 아직 끝나지 않은 모델도 호출 수에 포함된다. 돈 한도가 있으면 선택 단계의 허용 판단은 실행 중인 모델이 모두 끝날 때까지 취소 가능한 상태로 기다려 늦게 도착한 비용을 반영한다. 그래서 기본 설정(`chat.tool_mode: loop`, `limits.per_request_budget_usd: 0.03`)에서는 루프 1라운드의 도구가 함께 출발한 플래너 모델이 끝난 뒤에 시작한다. 늘어나는 시간은 두 모델의 응답 시간 차이만큼이고, 플래너가 재시도 대기에 걸리면 그만큼 길어진다(의도한 보수적 동작). 스스로 돈을 쓰는 도구(`Pays`: 그림·요약·운세처럼 모델을 부르는 도구, `search.web`, 검색으로 대신할 수 있는 `finance.quote`)도 끝난 뒤 다음 호출을 판정한다. 따라서 순차 실행보다 더 많은 선택 호출을 허용하지 않는다. 결과는 모델 순서대로 답변에 들어가고, 호출 기록(`tool_calls.cost_usd`)은 각 호출의 자식 `Guard`에 쌓인 비용만 적는다. 패닉은 해당 호출만 `unavailable`로 끝내며 부분 비용·실패 분류도 장부에 한 번 남긴다. 취소 후 아직 시작하지 않은 호출은 `cancelled` 결과를 남기고 호출 예약을 돌려준다.

**기억 검색은 플래너 직후에 시작한다.** 도구 루프가 아직 돌면 기억 검색은 루프 합류를 기다리지 않고 함께 돈다(호출 수 한도는 받지 않고 돈 한도만 받는다). 원문 질의 임베딩은 턴 시작에 미리 시작하고(`chat.QueryWarmer`, `engine.WarmQuery`) 플래너가 기억을 고르지 않으면 취소한다. 같은 질의의 호출은 하나의 요청을 함께 기다리고, 미리 만든 벡터는 검색이 쓸 때만 DB 캐시에 들어간다. 드라이런·연습 모드, `daily_limit`이 있는 임베딩 공급자, 기억 동의를 철회한 사람의 메시지는 미리 시작하지 않는다.

**한 번에 끝나는 도구(`tools.Builtin.Final`).** `weather.query`, `finance.fx`, `finance.quote`, `places.search`는 한 라운드의 호출이 전부 이런 도구이고 전부 원하는 값을 찾으면 루프를 끝낸다(`tool_loop.stop: final`). 실패, `status`가 `ok`가 아닌 결과, 검색으로 대신한 시세(`live: false`), 거절된 호출이 하나라도 있으면 지금처럼 다시 묻는다. `chat.loop_final: search`는 `search.web`도 같은 대우를 한다(본문 있는 결과 3건 이상, `depth: deep`이 아니고 메시지에 링크가 없을 때). `off`는 항상 묻는다.

**공급자별 동시 요청 상한.** 버스트에 엄격한 호스트는 프로세스 전체의 공유 governor로 동시에 보내는 요청 수를 묶는다(`application.providerGate`, `providers.Client.WithProviderGate`). 식별자는 공급자 종류(설정 항목의 별칭이 아닌 공통 공급자 ID)·기본 호스트·자격 증명의 비가역 해시이며, 같은 호스트·키의 중복 항목과 여러 Registry·Service가 자리를 공유한다. 설정 재로드와 상한 변경 뒤에도 이전 Registry의 실행 중 요청을 포함한다. 각 HTTP 시도는 성공·오류·패닉 모두에서 자리를 돌려주고 응답 본문을 닫으며, 재시도 대기는 자리 밖에서 한다. 이 상한은 동시에 진행 중인 요청 수를 제한하며 초당·분당 요청 수를 보장하는 rate limit은 아니다.

| 공급자 종류 | 동시 요청 | 근거(2026-10 문서) |
|---|---|---|
| `finnhub`, `kma`, `exchangerate` | 1 | Finnhub 무료 분당 약 60회, data.go.kr 게이트웨이 초당 한도, ExchangeRate-API 월 1,500회 |
| `kakao_search`, `open_meteo`, `linkup` | 2 | Kakao Local 일 10만(한도 초과는 429가 아니라 400 `-10`/`-11`), Open-Meteo 분당 600, Linkup 초당 10 |
| `jina_reader` | 키 없음 1, 키 있음 4 | 분당 20 / 500 |
| 그 밖(모델 호스트, Tavily·Exa·Serper) | 묶지 않음 | 공급자 게이트는 없다. 턴의 선택 작업은 최대 4개까지 함께 실행한다. 429는 `Retry-After`(30초 이하)를 따르고 없으면 지터를 둔 지수 대기로 재시도한다 |

## 4. config.yaml로 직접

```yaml
features:            # 기능 묶음 스위치(false면 그 기능의 도구 전부 꺼짐)
  search: true
tools:
  weather.query:
    defaults: {region: 부산}
    aliases: [날씨, w]
  search.web:
    chat: false
    limits: {per_user_per_day: 20, cooldown_sec: 10}
  chat.respond:
    limits: {per_user_per_day: 200}
guilds:
  "123":
    channels:
      "456":
        tools: {search.web: false, images.generate: true}   # 이 채널만
```

채널 `tools`의 `true`는 "이 채널에선 대화에도 쓴다"(도구 자체가 꺼져 있으면 여전히 안 됨), `false`는 "이 채널에선 안 쓴다".

## 5. 코드 없이 새 도구 붙이기 (HTTP 도구)

JSON을 돌려주는 https API라면 config만으로 도구가 된다. 플래너가 설명을 보고 부르고, `aliases`를 주면 `!명령`도 생긴다(뒤 텍스트가 첫 파라미터).

```yaml
tools:
  lotto.latest:                      # 이름: 소문자.소문자 (제품 Action과 겹치면 거부)
    description: "로또 당첨번호. parameters.round(회차, 모르면 생략)"
    route: "로또 번호면 lotto.latest"  # (선택) 플래너 규칙 2에 들어갈 한 줄
    aliases: [로또]
    params:
      - {name: round, required: false}
    http:
      url: "https://api.example.com/lotto?round={round}&key={credential}"
      credential_ref: env:LOTTO_API_KEY    # 키는 env/file로만. {credential}은 URL이나 headers에
      headers: {Accept: application/json}
      timeout_sec: 10
      max_chars: 3000                      # 모델에 넘기는 결과 길이 상한
      pick: [draw_no, numbers, date]       # (선택) 필요한 필드만 (a.b 경로)
    limits: {per_day: 200, cooldown_sec: 5}
```

- https만 허용한다(로컬 테스트용 `http://127.0.0.1`·`localhost` 제외). 호스트 부분에는 파라미터를 넣을 수 없다. 모델이 다른 서버로 요청을 보내게 만들 수 없다.
- `{param}` 값은 URL 인코딩된다. 선언하지 않은 `{이름}`은 설정 단계에서 거부된다.
- 키는 결과·오류 메시지에 나오지 않는다(오류 문구에서도 가린다).
- 결과는 `<도구결과>`로 답변 모델에 가고, "실제 값이니 바꾸지 말라"는 규칙을 따른다.

**코드로 내장 도구를 추가할 때:** Action을 만들고(`internal/catalog`, `internal/application`) → `internal/tools/tools.go` `Builtins()`에 한 줄(입력 목록 `Inputs`, 쓰는 공급자 종류 `Providers`, 모델·공개 웹 사용 `Uses`, 없으면 못 쓰는 모델 역할 `Role`(예: 이미지 생성은 `image`, 기억 검색은 `embed`), 사람들이 올린 것을 외부 서비스로 넘기는 도구면 `OptIn`까지. 콘솔의 시험 폼·기본값 폼·공급자 표시와 준비 상태(`tools.list`의 `ready`·`ready_reason`)는 모두 이것을 읽는다) → 언어팩 `tools.<id>.description`(+`route`)을 각 언어에 → Discord 명령이 필요하면 `internal/connectors/discord/commands.go`와 `locales/*.json`의 `discord.command.<name>.*`. 도구 목록을 따로 적는 곳은 이것뿐이다.

## 6. 언어

**어떤 언어로 말하나:** 채널 페르소나의 `locale` → 서버(`guilds.<id>.locale`) → `i18n.default_locale` 순으로 고른다. 기본 페르소나는 한국어 `warm_ko`(존댓말)·일본어 `warm_ja`(정중어)이고 영어는 `friendly_en`이다. `friendly_ko`·`friendly_ja`는 반말 프리셋이다(`masamong channels configure --guild_id G --id C --persona_ref friendly_en`). 플래너·답변 프롬프트, 기억 규칙, 도구 설명, 거절 메시지, Discord 카드 문구가 모두 그 언어로 나온다. 봇 이름은 `instance.bot_names`(첫 이름이 프롬프트에 쓰이고, 전부 호출 이름·비인물 처리).

**말투 기본값도 언어를 따른다:** 페르소나를 고르지 않은 방은 그 방 언어(서버 `guilds.<id>.locale` → `i18n.default_locale`)의 기본 말투를 쓴다. 한국어·일본어는 `i18n.polite_default`가 켜져 있으면 `warm_<tag>`, 아니면 `friendly_<tag>`이고, 없는 언어는 `friendly_en`, 그다음 `friendly_ko`로 내려간다(`DefaultPersonaCandidates`). 그래서 영어 서버 방에 한국어 말투가 붙지 않는다. **업그레이드:** `polite_default`는 새 인스턴스에서만 켜진다. 이 키가 없던 옛 인스턴스는 반말을 유지하고 운영자가 설정에서 켠다. `warm_ko`를 숨기면 한국어 방이 `friendly_ko`로 돌아간다. 새 언어는 `friendly_<tag>` 내장 말투만 추가하면 그 언어 방의 기본 말투가 된다(하이픈은 밑줄로: `zh-Hans` → `friendly_zh_hans`). 반말과 존댓말의 격차가 큰 언어는 `warm_<tag>`도 추가한다(그러면 `polite_default`가 켜진 인스턴스에서 그 언어의 기본이 된다). 내장 말투는 콘솔에서 삭제 대신 숨길 수 있고(`personas.<id>.hidden: true`) 언제든 되돌릴 수 있다. 콘솔 말투 화면은 화면·시스템 언어의 말투를 자동으로 보여주고(관리자별 수동 선택은 기억됨), 새 말투의 답변 언어와 예시 문구도 그 언어를 따른다.

**기억(검색) 언어:** `memory.languages: [ko]`(기본). 여러 개면 합친다(예: `[ko, en]` — 한국어 조사 처리 + 영어 단어 단위 색인·대명사). 토크나이저·조사·대명사·불용어·근거 생략 표시·요약 프롬프트·질의 지시문이 여기서 정해진다. 색인은 시작할 때 메모리에서 다시 만드므로 바꿔도 재임베딩은 필요 없다. 단, 질의 지시문이 바뀌면 질의 임베딩 캐시는 새로 쌓인다.

**새 언어 추가(기계 번역 포함):** 봇 언어팩 `internal/lang/packs/<tag>.json`, 운영·Discord 문구 `locales/<tag>.json`, 관리 콘솔 `locales/ui/<tag>.json`은 **서로 다른 번역 대상**이다. 콘솔의 언어 묶음은 프로그램에 내장되므로 새 언어는 다시 빌드해야 나타난다. 첫 설정의 `setup.resume`은 `locale`(기본은 콘솔 언어)을 받아 `i18n.default_locale`을 정한다. 기본 언어는 `en`·`ko`·`ja`·`zh-Hans`·`es`가 내장되어 있다(각 언어에 친근한·정중한·짧은·다정한 내장 말투 포함). 단순한 지역 변형은 기본 언어 태그를 파일명으로 쓴다(`fr-CA` 요청 → `fr`); 중국어 간체는 `zh-Hans`(`zh`·`zh-CN` 요청 포함), 번체는 별도 `zh-Hant`다. 콘솔은 필요할 때 지역별 JSON도 찾을 수 있다. 새 설정의 `bundled_locales` 기본값은 내장 언어팩에서 만들지만, 기존 설정의 목록이나 파일 한 곳만으로 모든 화면·운영 문구가 로드되었다고 볼 수 없다. `locales.list`의 `loaded`와 콘솔 언어 선택 목록을 각각 확인해야 한다.

- `internal/lang/packs/en.json`을 새 팩의 구조 기준으로 삼고 사용자에게 보이는 프롬프트·문구만 번역한다. `{bot}`·`{routing}` 같은 자리표시자의 **이름과 개수**, 플래너 JSON 예시의 키·타입, 도구 ID·`route`와 응답 계약을 보존한다. `search`의 조사·대명사·불용어·n-gram 설정, 기억 질의 지시문과 Discord 명령 별칭은 직역하지 말고 해당 언어 검색·명령 동작을 별도로 검토한다. 중국어·일본어의 `search.ngram_scripts`는 문자 체계에 맞게 결정한다. 검색 recipe가 바뀌면 샌드박스와 Go를 함께 검증한다.
- `locales/en.json`은 운영 문구의 기준이다. `{name}` 자리표시자와 중복 횟수를 보존한다. `locales/ui/en.json`은 콘솔 기준이며 `en`·`ko`만 `internal/transport/web/ui/scripts/strings.py`로 생성한다. 새 콘솔 번역은 `locales/ui/README.md`에 따라 별도 JSON으로 유지한다. 일부만 번역하면 영어로 fallback하지만, 복수형의 기본형과 변형은 한 묶음으로 번역한다.
- 원문 대화·사용자 ID·키·로컬 경로를 외부 번역기에 보내지 않는다. 실제 번역 서비스를 쓰면 **번역할 공개 기준 문구만** 전송하고 서비스의 보관 정책·요금·권한을 먼저 확인한다. 구조 검사는 의미·문화적 적합성·긴 문구 레이아웃을 보증하지 않으므로 사람이 화면·명령 응답·프롬프트와 검색 결과를 확인한다. 카카오톡 내보내기가 다른 언어 클라이언트에서 나온 경우에만 `internal/memory/corpus/markers/<tag>.json`(사진·삭제·입장 문구)을 추가하고 `imports start --source kakao_csv --export_locale <tag>`를 검증한다.
- 합성 새 언어를 쓰는 로더·API 테스트와 콘솔 검사(`cd internal/transport/web/ui && npm run test:i18n && npm run check -- --fail-on-warnings`)를 실행한 뒤 `make ui`, `go test ./internal/i18n ./internal/lang ./internal/transport/web`, `go test ./...`를 실행한다. 첫 실제 언어를 배포하기 전에는 그 언어로 로그인·언어 전환·오류/fallback·모바일(320px)·RTL(해당 시), 봇 대화와 기억 검색까지 확인한다. 기본 페르소나 `friendly_ko`는 한국어를 우선하므로 채널에 새 언어 페르소나를 명시적으로 지정한다.

**한국어 검색·답변 프롬프트는 바뀌지 않았다:** 플래너·문맥·기억·보안 규칙과 색인 요약 프롬프트는 옮기기 전과 바이트 단위로 같다(`internal/chat/testdata/golden_ko.json`, `internal/memory/search/testdata/golden_ko.json`). 샌드박스(`experimental/rag-leaf-parent/bot_plan.py`)와 같은 문장을 유지해야 측정한 품질이 그대로 적용되므로, 이 문장을 바꿀 때는 샌드박스도 함께 고치고 다시 측정한다. 유일한 의도된 차이는 플래너 규칙 2가 **켜진 도구만** 나열한다는 것. 2026-09-28에는 검색과 무관한 프롬프트(운세·별자리, 채널 요약, 먼저 말 걸기 판단)와 영어 팩 문장만 다듬었다. 샌드박스 parity(어휘 208질의, 검색 199문항×3)는 2026-09-23에 확인했다(§7).

## 7. 확인 방법·알려진 것

```bash
go test ./internal/tools/ ./internal/lang/ ./internal/chat/ ./internal/application/ -run 'Tool|Golden|English|ChatTurn|Operator'
```

**실사용 시나리오(실제 모델·실제 API).** `internal/application/testdata/scenarios_ko.json`의 52개 시나리오를 제품 전체(플래너 → 도구 → 답변, 기억은 연습 샘플)로 돌린다. 구 봇 로그의 질문 2,912개를 주제별로 센 비율(잡담 대부분, 그다음 날씨·주식·기억·그림·뉴스·장소·환율)을 따라 골랐고, 문장은 모두 새로 썼다(로그 원문을 옮기지 않음). 키 파일 폴더가 있어야 하고 몇 센트 든다.

```bash
MASAMONG_LIVE_KEYS_DIR=~/keys MASAMONG_SCENARIO_REPORT=/tmp/report.json \
  go test ./internal/application -run TestLiveScenarios -v -timeout 30m
# 일부만: MASAMONG_SCENARIO_ONLY=weather,tent_no_memory
```

2026-10-01 결과(CometAPI 모델): 첫 실행 45/50 → 고친 뒤 52/52, 한 번에 약 $0.04, 중앙값 6초. 이때 고친 것:
- 플래너 호출이 125초씩 멈추는 일 → 역할별 제한 시간(플래너 25초, 답변 70초)이 지나면 다른 대화 모델이 맡는다.
- "지금 대통령 누구야" 같은 바뀌는 사실을 모델 기억으로 답함 → `search.web` 경로 설명에 "시간이 지나면 바뀌는 사실" 추가.
- "뭐 할 수 있어?"가 `help.version`으로 안 감, "우산 챙겨야 돼?"가 날씨로 안 감 → 경로 설명 보강.
- "이번 주말"을 0/1, 2/3, 5/6으로 제각각 계산 → 기준 시각에 요일과 앞으로 7일의 `day_offset` 목록을 넣음(3/3 정확).
- 장소: 꾸밈말 검색 0건 → 다시 찾기, 지역 없는 검색 → 되묻기(위 표).
- 기억이 없을 때 "아마 너가 챙기기로 했던 것 같은데"처럼 누가 뭘 하기로 했는지 지어냄 → 기억 규칙에 "구체적인 일은 기억에 있을 때만", 빈 기억 안내문 수정(샌드박스와 Go 함께). 이후 "너였나?"처럼 묻는 사람을 찍는 답이 남은 것은 아래 "2차 점검"에서 고쳤다.

- 도구 연계 e2e는 가짜 모델 서버로 `TestChatTurnChainsPlannerToolsAndAnswer`(플래너→도구→답변, 꺼진 도구 미실행, 범위 키 제거, 자리표시자 거절), 언어는 `TestChatTurnSpeaksTheChannelPersonaLanguage`·`TestEnglishVoiceHasNoKoreanText`, 한도·예산은 `TestChatTurnLimitsAndMonthlyBudget`, HTTP 도구는 `TestOperatorHTTPTool`.
- **샌드박스 검색은 실행마다 결과가 조금 다를 수 있다.** 같은 입력으로 파이썬 덤프를 5번 만들면 discord 2문항의 후보 순서(8·22위 부근)가 바뀌는 실행이 있었다(질의 캐시를 꺼도 같음 — 부동소수 계산 순서로 추정). Go는 항상 같은 결과이고, 파이썬이 그렇게 흔들리지 않은 실행과는 222회 전부 일치했다. parity가 이 두 문항에서만 깨지면 먼저 파이썬 덤프를 다시 만들어 볼 것.

### 2026-10-01 추가 점검 (과적합·도구 실패)

- **도구가 답을 못 줘도 대화로 이어진다.** `!명령`·슬래시 명령이 대화형 도구를 불렀는데 답이 없으면(`application.Unanswered`: 실패, 또는 status `unconfigured`·`no_results`·`no_area`·`unavailable`·`not_found`·`error`) 오류 카드 대신 같은 메시지로 대화 턴을 돈다(`CommandToChat`). 실패한 결과를 넘기고 그 도구는 플래너 메뉴에서 빼서, 다른 도구(시세 → 검색)를 고르거나 대화로 잇는다. 권한 거부, 대화 안 하는 채널(`respond: off`), 비공개 명령, 대화 턴 자체 실패는 기존 카드.
- **대화 모델에는 "왜 안 되는지"를 넘기지 않는다**(`chatView`). 키 없음은 `unavailable`로만, 운영자 안내문·공급자 오류 문구는 빠진다. 모델이 "설정이 안 돼 있어서/키가 없어서"라고 말하던 문제. 카드·콘솔은 전체 결과를 그대로 본다.
- 빈 모델 응답도 실패로 보고 다른 대화 모델이 맡는다. 모델별 제한 시간은 `models.<role>.timeout_seconds`(기본 의도 30초·답변 90초, 콘솔 연결 → 모델 → 단가 설정 창).
- 장소 재검색은 단어 순서에 기대지 않는다: 각 단어 + 가게 종류로 최대 2번, 카카오가 지역을 읽은 결과만 받는다.
- **과적합 점검:** 고친 뒤 새로 쓴 문장 28개(홀드아웃)를 돌려 28/28, 키를 일부러 뺀(finnhub·kakao·linkup) 실패 시나리오 9개는 2회 연속 9/9(대화로 이어짐, 관리자용 말 없음, 정상 명령은 카드 유지). 홀드아웃 파일은 `.masamong-scratch/holdout-*.json`(커밋 안 함, 튜닝에 쓰지 말 것). 실행: `MASAMONG_SCENARIO_FILE=… MASAMONG_SCENARIO_DROP=finnhub,kakao,linkup`.

### 2026-10-01 2차 점검

- **기억이 없을 때 묻는 사람을 찍지 않는다.** 원인은 규칙끼리 부딪힌 것: "기억이 없으면 되물어도 된다"와 예시 "그거 누구였더라?"를 모델이 "너 아니었어?/너였나?"로 묻는 사람에게 되물었다. 규칙과 빈 기억 안내문(`chat.rag_notes.empty_in_allowed_scope`)을 "누구도 후보로 올리지 말고, 기억 안 난다고 가볍게 말한 뒤 아는 사람 있는지 방에 물어봐라"로 바꿨다(샌드박스 `engine.py`·`bot_runtime.py`와 Go 한국어 팩 같은 문구, 영어 팩도 같은 뜻으로, 골든 갱신). 측정: 새로 쓴 누가-질문 8개 × 3회에서 묻는 사람·봇 자신을 찍은 답 10/24 → 0/24. 바꾼 뒤 처음 쓴 문장 6개 × 2회(`.masamong-scratch/holdout-who-20261001.json`)도 0/12. 기억이 있는 질문은 그대로 기억으로 답함(메인·홀드아웃 기억 시나리오 전부 통과).
- **도움말은 지금 답할 수 있는 도구만 안내한다.** `help.version`의 대화 도구·`!` 명령 목록에서 준비 안 된 도구(키·모델 없음, 꺼짐)를 뺀다 — 도구 페이지의 준비 상태(`toolReady`)와 같은 판단. `TestHelpListsOnlyReadyTools`.
- 장소 `no_area` 오탐: 위 표처럼 "결과 대부분" 기준으로 바꿨다(`places_test.go`의 "동네 카페").
- 빈 응답 폴백 이후 전체 재실행: 메인 52/52, 홀드아웃 28/28, 새 누가-질문 12/12.
- **새 Discord 원문 가져오기 확인**(임시 데이터 폴더, 운영 인스턴스 아님): 이전 원문(58,519건)을 넣은 뒤 새 원문(61,254건)을 `imports start --source raw`로 넣으면 2,608건이 새로 들어가고 56,328건은 중복으로 건너뛴다(늘어난 2,735건 중 127건은 사람 글이 아니거나 시각이 없어 제외). 같은 파일을 두 번 넣어도 0건 추가. 운영 인스턴스에 넣는 건 콘솔 가져오기로 운영자가 할 일(넣은 뒤 기억 다시 만들기 비용 발생).

### 2026-10-05 감사 뒤 점검 (말투·기억·요약)

- **기억이 흐릿할 때 지어내지 않는다.** `insufficient_authorized_candidates` 안내문은 "분명히 기억나지 않으니 방 사람들에게 물어보고, 누가 뭘 했는지도, 봇 자신의 사정도 지어내지 마라"고 말한다(5개 언어). 기억 규칙에서는 "분위기는 조금 틀려도 넘어가라"는 문장을 빼고, "네 사정을 지어내지 마라(예: 나 그날 정신없었나 봐)" 규칙을 모든 언어에 넣었다. 52개 시나리오를 실제 모델로 돌려 기억 5/5, 기억이 없는 시나리오에서 지어낸 사정 없음(전체 51/52, 비용 약 $0.02. 유일한 실패 `next_monday`는 이후 날씨 도구가 14일 예보를 받게 고쳐 통과함). **더 큰 누가-질문 묶음은 다시 돌리지 않았다.**
- **`seoa_role`의 기대값은 "고기" 또는 "숙소" 둘 다 받는다**(샘플에서 서아의 역할이 둘이라서, `answer_any`). 여러 기억이 걸리면 각각 말하거나 어느 일인지 되묻는다는 규칙이 `memory_rules`에 있다.
- **한 가지 말투.** 아침 운세·별자리 프롬프트는 언어 기본 말투와 같은 어조(한국어 해요체, 일본어 です・ます)로 쓰고, 기억 규칙의 예시는 말투에 중립적인 "~했잖아(요)"로 바꿨다. 예전에는 프롬프트가 반말로 쓰여 있고 인사는 `{name}님`이라 어조가 섞였다.
- **`/summary`는 `##` 제목 대신 굵은 소제목과 글머리표를 쓴다**(`channel_summary`, `channel_summary_incremental`). 코드가 `## 핵심 3줄`을 읽는 곳은 없다(가짜 모델 출력을 쓰는 테스트 픽스처뿐).
- **도움말은 지금 살아 있는 것만 안내한다.** 도구가 준비되었는지와 슬래시 명령이 실제로 등록되었는지를 함께 보고(`TestHelpListsOnlyReadyLiveSurfaces`), 내 언어의 접두사 단어와 메시지 메뉴의 "답변 평가"를 넣는다.
- 알려진 한계: 내장 말투의 문구(`friendly_en`, `warm_ko` 등)는 팩의 기본 `default_persona`와 달리 "상대방의 언어로 답하라"고 말하지 않는다. 한국어 화자 이름과 한국어 기억이 있는 방에 영어·일본어로 물으면 한국어로 답하는 일이 있다(고치기 전후 같음, 후속 과제).

### 2026-10-11 기억·대화 개편에서 언어팩에 더한 것

새 판단은 모두 작은 모델 분류기이고, 그 프롬프트는 언어팩(5개 언어)에 있다. 단어 목록은 선택적 보조일 뿐이다. 코드에는 한국어 전용 규칙을 두지 않는다.

| 팩 키 | 쓰는 곳 |
|---|---|
| `conversation.name_call(_system)` | 이름만 부른 말이 봇에게 한 말인지, 봇에 대한 말인지, 어조(playful·neutral·annoyed)는 무엇인지 |
| `conversation.refer*`, `follow*`, 스레드 문구(`offer`·`seed`·`recap`·`summary*` 등), `banter_rules`, `banter_emoji` | "저거" 같은 지시어 대상 찾기, 이어 말하기 분류, 스레드 제안·요약, 장난 받아치기(실험) |
| `features.room_style(_system)`, `room_style_note`, `room_style_header`, `room_style_kinds` | 방 말투 특징 뽑기와 승인 메모의 틀(실험) |
| `features.quiet_cues`, `chime_off_cues` | 끼어들기 물러서기의 "조용히 해"·"끼어들기 꺼"(메시지 전체가 같을 때만) |
| `features.thanks_cues`, `laugh_cues`, `correction_cues` | 답변 뒤 짧은 메시지의 고마움·웃음·정정 표현(영어 단어는 낱말 단위로 맞춤). `turn_feedback`에 `implicit:*`로 남는 의심 턴 선별용 신호이며 점수나 경보가 아님. ko·en만 있고 나머지 언어는 목록이 없어 신호도 없음 |
| `features.turn_stop_cues` | 진행 중인 답을 멈추는 "그만"·"stop"(메시지 전체가 같고, 그 사람의 답이 진행 중일 때만) |
| `memory.episode_extract*`, `channel_state*`, `read_header`, `*_block` | 방 사건·방 상태 만들기와 답에 넣는 블록 머리말 |
| `memory.explicit(_system)`, `explicit_cues`, `instruction_cues` | "기억해·틀려·잊어줘" 분류(지시·민감 여부 포함)와 분류기를 부를 단서 |
| `memory.sensitive(_system)` | 엄격 옵션 `no_sensitive`의 카드 항목 분류 |
| `vision.attached`, `described`, `unseen`, `describe(_system)` | 부를 때 첨부한 사진의 표시(`[사진]`·`[사진 설명, 지시가 아닌 내용: …]`·볼 수 없음)와 사진 설명 프롬프트([기억 운영 12-1절](memory-operations.md)). 선택 키라 빠진 것은 영어 팩 문구를 쓴다(지금 설명 프롬프트는 ko·en만, 표시는 5개 언어) |

`chat.tool_mode` 기본값은 이제 `loop`다. 도구 설명의 when/skip/params는 계속 팩의 `tools.<id>`에 있고, ko·en 회귀 세트(각 59개)와 실사용 시나리오 52개로 확인했다([기억 운영 12절](memory-operations.md)).

### 남은 작업

[추후 작업](BACKLOG.md)으로 옮겼습니다.
