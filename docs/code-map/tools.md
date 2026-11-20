# 채팅 도구 색인

모델이 보는 내장 도구 목록: 호출하는 Action과 그 도구를 설명하는 언어팩 키.

이 파일은 생성됩니다: go run ./tools/codemap (손으로 고치지 마세요)

원본: `internal/tools/tools.go`의 `Builtins()` (도구 ID = catalog Action ID), `internal/lang/packs/ko.json`의 `tools.<id>.*` (다른 팩은 부분 집합을 가질 수 있음)

| 도구 ID | Action | 언어팩 키 (tools.<id>.아래) |
|---|---|---|
| activity.ranking | activity.ranking | description · route · purpose · when · params.period |
| chat.respond | chat.respond | — |
| disasters.query | disasters.query | description · purpose · when · skip · params.days |
| finance.fx | finance.fx | description · route · purpose · when · skip · params.amount · params.base · params.symbols |
| finance.quote | finance.quote | description · route · purpose · when · skip · params.krw · params.symbol |
| fortune.query | fortune.query | — |
| fortune.subscribe | fortune.subscribe | — |
| help.version | help.version | description · route · purpose · when |
| images.generate | images.generate | description · purpose · when · skip · params.aspect_ratio · params.prompt |
| memory.search | memory.search | description |
| notices.status | notices.status | — |
| notices.subscribe | notices.subscribe | — |
| places.search | places.search | description · route · purpose · when · skip · params.query · params.size |
| polls.close | polls.close | — |
| polls.create | polls.create | description · route · purpose · when · skip · params.allow_multiselect · params.duration_hours · params.options_text · params.question |
| polls.result | polls.result | — |
| polls.vote | polls.vote | — |
| reminders.propose | reminders.propose | description · route · purpose · when · skip · params.ask · params.count · params.day · params.every · params.kind · params.repeat · params.task · params.until · params.weekdays · params.what · params.when · params.with_context |
| reminders.update | reminders.update | description · route · purpose · when · skip · params.id · params.match · params.op |
| search.web | search.web | description · route · purpose · when · skip · params.depth · params.fresh · params.queries · params.query · params.size · params.topic |
| summaries.channel | summaries.channel | description · route · purpose · when · skip · params.refresh |
| transfer.recent | transfer.recent | — |
| weather.query | weather.query | description · route · purpose · when · skip · params.day_offset · params.region |
| web.read | web.read | description · route · purpose · when · skip · params.max_chars · params.url |
