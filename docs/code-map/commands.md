# Discord 명령 색인

Discord에 등록하는 애플리케이션 명령: 한국어 이름, 호출하는 Action과 옵션. 각 명령은 `!` 접두 별칭으로도 부를 수 있고, 등록하지 않는 접두 전용 명령 `fortune-month`·`fortune-year`·`subscribe`은(는) 이 표에 없습니다.

이 파일은 생성됩니다: go run ./tools/codemap (손으로 고치지 마세요)

원본: `internal/connectors/discord`의 `ApplicationCommands()` (명령·옵션), `locales/*.json`의 `discord.command.*` (한국어 이름)

| 명령 | 한국어 이름 | 종류 | Action | 옵션 (이름, 한국어, 타입, 필수) |
|---|---|---|---|---|
| `Rate this reply` | 답변 평가 | 메시지 메뉴 | — | — |
| `admin` | 관리 | 슬래시 | access.policy | op (할일, string, 선택)<br>mode (방식, string, 선택)<br>user (대상, user, 선택) |
| `alerts` | 특보 | 슬래시 | disasters.query | — |
| `forgetme` | 잊어줘 | 슬래시 | memory.mine.forget | — |
| `fortune` | 운세 | 슬래시 | fortune.query | what (보기, string, 선택)<br>time (시간, string, 선택) |
| `fortune-register` | 운세등록 | 슬래시 | fortune.subscribe | birth_date (생년월일, string, 필수)<br>birth_time (태어난시간, string, 선택)<br>gender (성별, string, 선택)<br>birth_place (출생지, string, 선택) |
| `fx` | 환율 | 슬래시 | finance.fx | currency (통화, string, 선택)<br>base (기준통화, string, 선택)<br>amount (금액, number, 선택) |
| `help` | 도움말 | 슬래시 | help.version | — |
| `image` | 이미지 | 슬래시 | images.generate | prompt (장면, string, 필수) |
| `mymemory` | 내기억 | 슬래시 | memory.mine.show | — |
| `mysettings` | 내설정 | 슬래시 | preferences.set | region (지역, string, 선택)<br>timezone (시간대, string, 선택)<br>quiet (방해금지, string, 선택)<br>language (언어, string, 선택) |
| `notices` | 공지 | 슬래시 | notices.status | what (보기, string, 선택)<br>value (값, string, 선택) |
| `notices-register` | 공지등록 | 슬래시 | notices.subscribe | school (학교, string, 필수)<br>degree (과정, string, 선택)<br>grade (학년, integer, 선택)<br>department (학과, string, 선택)<br>campus (캠퍼스, string, 선택)<br>interests (관심주제, string, 선택)<br>time (시간, string, 선택) |
| `places` | 장소 | 슬래시 | places.search | query (검색어, string, 필수) |
| `poll` | 투표 | 슬래시 | polls.create | question (질문, string, 필수)<br>answers (항목, string, 선택)<br>duration (기간, integer, 선택)<br>multiselect (복수선택, boolean, 선택) |
| `privacy` | 개인정보 | 슬래시 | privacy.consent | action (할일, string, 선택)<br>scope (목적, string, 선택) |
| `quote` | 시세 | 슬래시 | finance.quote | symbol (종목, string, 필수) |
| `ranking` | 랭킹 | 슬래시 | activity.ranking | period (기간, string, 선택) |
| `remind` | 알림 | 슬래시 | reminders.propose | when (언제, string, 필수)<br>what (내용, string, 선택)<br>repeat (반복, string, 선택) |
| `reminders` | 알림목록 | 슬래시 | reminders.update | — |
| `search` | 검색 | 슬래시 | search.web | query (검색어, string, 필수) |
| `status` | 상태 | 슬래시 | runtime.doctor | — |
| `summary` | 요약 | 슬래시 | summaries.channel | — |
| `transfer` | 편입 | 슬래시 | transfer.recent | what (보기, string, 선택)<br>value (값, string, 선택) |
| `updates` | 업데이트 | 슬래시 | updates.notes | what (할일, string, 선택)<br>value (값, string, 선택) |
| `weather` | 날씨 | 슬래시 | weather.query | region (지역, string, 선택) |
| `zodiac` | 별자리 | 슬래시 | fortune.query | sign (별자리, string, 선택) |
