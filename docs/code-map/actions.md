# Action 색인

internal/catalog의 모든 Action: CLI 명령, 콘솔 경로와 internal/application에서의 처리 위치.

이 파일은 생성됩니다: go run ./tools/codemap (손으로 고치지 마세요)

원본: `internal/catalog/catalog.go`의 `catalog.All()` (ID·상태·효과·CLI·경로), `internal/application`의 `case` 절과 맵 항목 (go/parser로 읽은 처리 위치)

| Action | 상태 | 효과 | CLI 명령 | 콘솔 경로 | 처리 위치 |
|---|---|---|---|---|---|
| access.policy | executable | internal_write | `access` | `/people` | [access.go:343](../../internal/application/access.go)<br>[admin.go:955](../../internal/application/admin.go)<br>[service.go:750](../../internal/application/service.go) |
| activity.ranking | executable | read | `activity ranking` | `/tools` | [service.go:694](../../internal/application/service.go) |
| admin.prefs | executable | read | `prefs show` | `/` | [admin.go:321](../../internal/application/admin.go)<br>[service.go:772](../../internal/application/service.go) |
| admin.prefs.set | executable | internal_write | `prefs set` | `/` | [admin.go:321](../../internal/application/admin.go)<br>[admin.go:870](../../internal/application/admin.go)<br>[service.go:772](../../internal/application/service.go) |
| agent.simulate | executable | read | `agent simulate` | `/lab` | [agent_backup.go:23](../../internal/application/agent_backup.go)<br>[service.go:742](../../internal/application/service.go) |
| alerts.status | executable | read | `alerts status` | `/maintenance` | [service.go:784](../../internal/application/service.go) |
| alerts.test | executable | external_write | `alerts test` | `/maintenance` | [service.go:553](../../internal/application/service.go)<br>[service.go:784](../../internal/application/service.go) |
| audit.list | executable | read | `audit` | `/settings` | [admin.go:337](../../internal/application/admin.go)<br>[service.go:773](../../internal/application/service.go) |
| backup.create | executable | internal_write | `backup create` | `/maintenance` | [backup.go:150](../../internal/application/backup.go)<br>[service.go:776](../../internal/application/service.go) |
| backup.delete | executable | internal_write | `backup delete` | `/maintenance` | [backup.go:161](../../internal/application/backup.go)<br>[service.go:776](../../internal/application/service.go) |
| backup.list | executable | read | `backup list` | `/maintenance` | [backup.go:148](../../internal/application/backup.go)<br>[service.go:776](../../internal/application/service.go) |
| backup.restore | executable | internal_write | `backup restore` | `/maintenance` | [backup.go:194](../../internal/application/backup.go)<br>[service.go:552](../../internal/application/service.go)<br>[service.go:776](../../internal/application/service.go) |
| backup.restoreDryRun | executable | read | `backup restore-preview` | `/maintenance` | [backup.go:174](../../internal/application/backup.go)<br>[service.go:776](../../internal/application/service.go) |
| backup.verify | executable | read | `backup verify` | `/maintenance` | [backup.go:174](../../internal/application/backup.go)<br>[service.go:776](../../internal/application/service.go) |
| channels.attachments.add | executable | internal_write | `channels attachments add` | `/rooms` | [channels.go:37](../../internal/application/channels.go)<br>[service.go:702](../../internal/application/service.go) |
| channels.attachments.remove | executable | internal_write | `channels attachments remove` | `/rooms` | [channels.go:39](../../internal/application/channels.go)<br>[service.go:702](../../internal/application/service.go) |
| channels.configure | executable | internal_write | `channels configure` | `/rooms` | [channels.go:31](../../internal/application/channels.go)<br>[service.go:702](../../internal/application/service.go) |
| channels.invite | executable | read | `channels invite` | `/connections` | [channels.go:33](../../internal/application/channels.go)<br>[service.go:702](../../internal/application/service.go) |
| channels.leave | executable | external_write | `channels leave` | `/rooms` | [admin.go:970](../../internal/application/admin.go)<br>[channels.go:35](../../internal/application/channels.go)<br>[service.go:702](../../internal/application/service.go) |
| channels.list | executable | read | `channels list` | `/rooms` | [channels.go:29](../../internal/application/channels.go)<br>[service.go:702](../../internal/application/service.go) |
| channels.ops_log | executable | read | `channels ops-log` | `/rooms` | [channels.go:41](../../internal/application/channels.go)<br>[service.go:702](../../internal/application/service.go) |
| channels.permissions | executable | read | `channels permissions` | `/rooms` | [service.go:780](../../internal/application/service.go) |
| channels.view.attach | executable | internal_write | `channels view attach` | `/rooms` | [channels.go:45](../../internal/application/channels.go)<br>[service.go:702](../../internal/application/service.go) |
| channels.view.detach | executable | internal_write | `channels view detach` | `/rooms` | [channels.go:47](../../internal/application/channels.go)<br>[service.go:702](../../internal/application/service.go) |
| channels.view.show | executable | read | `channels view show` | `/rooms` | [channels.go:43](../../internal/application/channels.go)<br>[service.go:702](../../internal/application/service.go) |
| chat.respond | executable | external_read | `chat` | `/lab` | [admin.go:870](../../internal/application/admin.go)<br>[memory.go:699](../../internal/application/memory.go)<br>[service.go:720](../../internal/application/service.go) |
| config.apply | executable | internal_write | `config apply` | `/settings` | [service.go:656](../../internal/application/service.go) |
| config.changes | executable | read | `config changes` | `/settings` | [admin.go:349](../../internal/application/admin.go)<br>[service.go:774](../../internal/application/service.go) |
| config.diff | executable | read | `config diff` | `/settings` | [service.go:654](../../internal/application/service.go) |
| config.document | executable | read | `config document` | `/settings` | [admin.go:343](../../internal/application/admin.go)<br>[service.go:773](../../internal/application/service.go) |
| config.patch | executable | internal_write | `config patch` | `/settings` | [admin.go:345](../../internal/application/admin.go)<br>[service.go:773](../../internal/application/service.go) |
| config.rollback | executable | internal_write | `config rollback` | `/settings` | [service.go:658](../../internal/application/service.go) |
| config.show | executable | read | `config show` | `/settings` | [service.go:652](../../internal/application/service.go) |
| config.snapshot | executable | internal_write | `config snapshot save` | `/settings` | [admin.go:347](../../internal/application/admin.go)<br>[ops_extra.go:126](../../internal/application/ops_extra.go)<br>[service.go:774](../../internal/application/service.go) |
| config.snapshot.delete | executable | internal_write | `config snapshot delete` | `/settings` | [admin.go:347](../../internal/application/admin.go)<br>[ops_extra.go:137](../../internal/application/ops_extra.go)<br>[service.go:774](../../internal/application/service.go) |
| config.snapshot.restore | executable | internal_write | `config snapshot restore` | `/settings` | [admin.go:347](../../internal/application/admin.go)<br>[ops_extra.go:142](../../internal/application/ops_extra.go)<br>[service.go:774](../../internal/application/service.go) |
| config.snapshots | executable | read | `config snapshots` | `/settings` | [admin.go:347](../../internal/application/admin.go)<br>[service.go:774](../../internal/application/service.go) |
| config.validate | executable | read | `config validate` | `/settings` | [service.go:654](../../internal/application/service.go) |
| conversation.log | executable | read | `conversation log` | `/rooms` | [service.go:760](../../internal/application/service.go) |
| dashboard.overview | executable | read | `dashboard` | `/` | [admin.go:327](../../internal/application/admin.go)<br>[service.go:772](../../internal/application/service.go) |
| disasters.alerts | executable | read | `disasters alerts` | `/tools` | [service.go:700](../../internal/application/service.go) |
| disasters.query | executable | external_read | `disasters` | `/tools` | [service.go:668](../../internal/application/service.go) |
| discord.check | executable | internal_write | `discord check` | `/connections` | [admin.go:964](../../internal/application/admin.go)<br>[service.go:698](../../internal/application/service.go) |
| eval.run | executable | external_read | `eval run` | `/lab` | [service.go:706](../../internal/application/service.go) |
| eval.runs | executable | read | `eval runs` | `/lab` | [service.go:706](../../internal/application/service.go) |
| events.timeline | executable | read | `events` | `/memory` | [service.go:738](../../internal/application/service.go) |
| feedback.summary | executable | read | `feedback summary` | `/turns` | [service.go:788](../../internal/application/service.go) |
| finance.fx | executable | external_read | `finance fx` | `/tools` | [service.go:686](../../internal/application/service.go)<br>[tools.go:218](../../internal/application/tools.go) |
| finance.quote | executable | external_read | `finance quote` | `/tools` | [service.go:686](../../internal/application/service.go)<br>[tools.go:569](../../internal/application/tools.go) |
| fortune.query | executable | external_read | `fortune` | `/tools` | [service.go:670](../../internal/application/service.go) |
| fortune.subscribe | executable | internal_write | `fortune subscribe` | `/people` | [admin.go:960](../../internal/application/admin.go)<br>[service.go:672](../../internal/application/service.go) |
| help.version | executable | read | `help` | `/` | [service.go:662](../../internal/application/service.go) |
| images.generate | executable | external_write | `images` | `/tools` | [service.go:684](../../internal/application/service.go) |
| imports.preview | executable | read | `imports preview` | `/blocks` | [service.go:718](../../internal/application/service.go) |
| imports.start | executable | internal_write | `imports start` | `/blocks` | [memory.go:641](../../internal/application/memory.go)<br>[service.go:720](../../internal/application/service.go) |
| instance.reset | executable | internal_write | `reset` | `/maintenance` | [service.go:554](../../internal/application/service.go)<br>[service.go:790](../../internal/application/service.go) |
| jobs.cancel | executable | internal_write | `jobs cancel` | `/jobs` | [jobs_actions.go:31](../../internal/application/jobs_actions.go)<br>[service.go:660](../../internal/application/service.go) |
| jobs.get | executable | read | `jobs get` | `/jobs` | [jobs_actions.go:24](../../internal/application/jobs_actions.go)<br>[service.go:660](../../internal/application/service.go) |
| jobs.list | executable | read | `jobs list` | `/jobs` | [jobs_actions.go:14](../../internal/application/jobs_actions.go)<br>[service.go:660](../../internal/application/service.go) |
| jobs.retry | executable | internal_write | `jobs retry` | `/jobs` | [jobs_actions.go:65](../../internal/application/jobs_actions.go)<br>[service.go:660](../../internal/application/service.go) |
| jobs.watch | executable | read | `jobs watch` | `/jobs` | [jobs_actions.go:55](../../internal/application/jobs_actions.go)<br>[service.go:660](../../internal/application/service.go) |
| lab.review | executable | internal_write | `lab review` | `/lab` | [admin.go:341](../../internal/application/admin.go)<br>[admin.go:870](../../internal/application/admin.go)<br>[service.go:773](../../internal/application/service.go) |
| lab.reviews | executable | read | `lab reviews` | `/lab` | [admin.go:341](../../internal/application/admin.go)<br>[service.go:773](../../internal/application/service.go) |
| locales.list | executable | read | `locales` | `/persona` | [service.go:664](../../internal/application/service.go) |
| memory.backfill | executable | internal_write | `memory backfill` | `/rooms` | [admin.go:962](../../internal/application/admin.go)<br>[service.go:722](../../internal/application/service.go) |
| memory.blocks.delete | executable | internal_write | `memory blocks delete` | `/blocks` | [memory_blocks.go:119](../../internal/application/memory_blocks.go)<br>[service.go:704](../../internal/application/service.go) |
| memory.blocks.list | executable | read | `memory blocks list` | `/blocks` | [memory_blocks.go:80](../../internal/application/memory_blocks.go)<br>[service.go:704](../../internal/application/service.go) |
| memory.blocks.prepare | executable | external_write | `memory blocks prepare` | `/blocks` | [admin.go:966](../../internal/application/admin.go)<br>[memory_blocks.go:115](../../internal/application/memory_blocks.go)<br>[service.go:704](../../internal/application/service.go) |
| memory.blocks.show | executable | read | `memory blocks show` | `/blocks` | [memory_blocks.go:98](../../internal/application/memory_blocks.go)<br>[service.go:704](../../internal/application/service.go) |
| memory.blocks.update | executable | internal_write | `memory blocks update` | `/blocks` | [memory_blocks.go:117](../../internal/application/memory_blocks.go)<br>[service.go:704](../../internal/application/service.go) |
| memory.episodes.extract | executable | external_write | `memory episodes extract` | `/blocks` | [admin.go:966](../../internal/application/admin.go)<br>[episodes_actions.go:75](../../internal/application/episodes_actions.go)<br>[service.go:710](../../internal/application/service.go) |
| memory.episodes.forget | executable | internal_write | `memory episodes forget` | `/memory` | [episodes_actions.go:71](../../internal/application/episodes_actions.go)<br>[service.go:710](../../internal/application/service.go) |
| memory.episodes.list | executable | read | `memory episodes list` | `/memory` | [episodes_actions.go:67](../../internal/application/episodes_actions.go)<br>[service.go:710](../../internal/application/service.go) |
| memory.episodes.show | executable | read | `memory episodes show` | `/memory` | [episodes_actions.go:69](../../internal/application/episodes_actions.go)<br>[service.go:710](../../internal/application/service.go) |
| memory.mine.forget | executable | internal_write | `memory mine forget` | `/memory` | [access.go:343](../../internal/application/access.go)<br>[admin.go:968](../../internal/application/admin.go)<br>[explicit.go:893](../../internal/application/explicit.go)<br>[service.go:712](../../internal/application/service.go) |
| memory.mine.show | executable | read | `memory mine show` | `/memory` | [access.go:343](../../internal/application/access.go)<br>[explicit.go:837](../../internal/application/explicit.go)<br>[service.go:712](../../internal/application/service.go) |
| memory.reindex | executable | external_write | `memory reindex` | `/blocks` | [memory.go:684](../../internal/application/memory.go)<br>[service.go:720](../../internal/application/service.go) |
| memory.review | executable | internal_write | `memory review` | `/memory` | [service.go:708](../../internal/application/service.go) |
| memory.reviews | executable | read | `memory reviews` | `/memory` | [service.go:708](../../internal/application/service.go) |
| memory.search | executable | read | `memory search` | `/memory` | [memory.go:604](../../internal/application/memory.go)<br>[service.go:720](../../internal/application/service.go)<br>[tools.go:61](../../internal/application/tools.go) |
| memory.state.show | executable | read | `memory state show` | `/memory` | [episodes_actions.go:73](../../internal/application/episodes_actions.go)<br>[service.go:710](../../internal/application/service.go) |
| messages.search | executable | read | `messages search` | `/memory` | [memory.go:564](../../internal/application/memory.go)<br>[service.go:720](../../internal/application/service.go) |
| migration.apply | executable | internal_write | `migration apply` | `/maintenance` | [admin.go:970](../../internal/application/admin.go)<br>[service.go:680](../../internal/application/service.go) |
| models.embed_width | executable | internal_write | `models embed-width` | `/connections` | [providers_persona.go:102](../../internal/application/providers_persona.go)<br>[service.go:696](../../internal/application/service.go) |
| models.list | executable | read | `models list` | `/connections` | [providers_persona.go:162](../../internal/application/providers_persona.go)<br>[service.go:696](../../internal/application/service.go) |
| models.recommend | executable | internal_write | `models recommend` | `/connections` | [admin.go:964](../../internal/application/admin.go)<br>[providers_persona.go:100](../../internal/application/providers_persona.go)<br>[service.go:696](../../internal/application/service.go) |
| models.select | executable | internal_write | `models select` | `/connections` | [providers_persona.go:208](../../internal/application/providers_persona.go)<br>[service.go:696](../../internal/application/service.go) |
| notices.status | executable | read | `notices status` | `/tools` | [service.go:674](../../internal/application/service.go) |
| notices.subscribe | executable | internal_write | `notices subscribe` | `/people` | [admin.go:960](../../internal/application/admin.go)<br>[service.go:676](../../internal/application/service.go) |
| participation.mode | executable | internal_write | `participation` | `/rooms` | [admin.go:947](../../internal/application/admin.go)<br>[service.go:758](../../internal/application/service.go) |
| people.list | executable | read | `people list` | `/memory` | [service.go:714](../../internal/application/service.go) |
| people.merge | executable | internal_write | `people merge` | `/memory` | [service.go:714](../../internal/application/service.go) |
| people.servers | executable | read | `people servers` | `/people` | [service.go:716](../../internal/application/service.go) |
| personas.apply | executable | internal_write | `personas apply` | `/persona` | [channels.go:663](../../internal/application/channels.go)<br>[service.go:740](../../internal/application/service.go) |
| personas.list | executable | read | `personas list` | `/persona` | [channels.go:661](../../internal/application/channels.go)<br>[service.go:740](../../internal/application/service.go) |
| places.search | executable | external_read | `places search` | `/tools` | [service.go:692](../../internal/application/service.go) |
| polls.close | executable | internal_write | `polls close` | `/tools` | [features.go:1254](../../internal/application/features.go)<br>[service.go:682](../../internal/application/service.go) |
| polls.create | executable | internal_write | `polls create` | `/tools` | [features.go:1164](../../internal/application/features.go)<br>[service.go:682](../../internal/application/service.go) |
| polls.result | executable | read | `polls result` | `/tools` | [features.go:1267](../../internal/application/features.go)<br>[service.go:682](../../internal/application/service.go) |
| polls.vote | executable | internal_write | `polls vote` | `/tools` | [features.go:1233](../../internal/application/features.go)<br>[service.go:682](../../internal/application/service.go) |
| preferences.set | executable | internal_write | `preferences` | `/people` | [access.go:343](../../internal/application/access.go)<br>[admin.go:953](../../internal/application/admin.go)<br>[service.go:748](../../internal/application/service.go) |
| privacy.consent | executable | internal_write | `privacy` | `/people` | [access.go:343](../../internal/application/access.go)<br>[admin.go:951](../../internal/application/admin.go)<br>[service.go:746](../../internal/application/service.go) |
| providers.add | executable | internal_write | `providers add` | `/connections` | [providers_persona.go:70](../../internal/application/providers_persona.go)<br>[service.go:696](../../internal/application/service.go) |
| providers.credentials | executable | internal_write | `providers credentials` | `/connections` | [providers_persona.go:136](../../internal/application/providers_persona.go)<br>[service.go:553](../../internal/application/service.go)<br>[service.go:696](../../internal/application/service.go) |
| providers.list | executable | read | `providers list` | `/connections` | [providers_persona.go:29](../../internal/application/providers_persona.go)<br>[service.go:696](../../internal/application/service.go) |
| providers.remove | executable | internal_write | `providers remove` | `/connections` | [providers_persona.go:72](../../internal/application/providers_persona.go)<br>[service.go:696](../../internal/application/service.go) |
| providers.test | executable | read | `providers test` | `/connections` | [providers_persona.go:104](../../internal/application/providers_persona.go)<br>[service.go:696](../../internal/application/service.go) |
| reminders.propose | executable | internal_write | `reminders propose` | `/rooms` | [service.go:754](../../internal/application/service.go) |
| reminders.update | executable | internal_write | `reminders` | `/rooms` | [admin.go:949](../../internal/application/admin.go)<br>[service.go:756](../../internal/application/service.go) |
| reports.create | executable | read | `reports create` | `/stats` | [service.go:744](../../internal/application/service.go) |
| room.style.build | executable | external_write | `room style build` | `/rooms` | [admin.go:966](../../internal/application/admin.go)<br>[roomstyle.go:701](../../internal/application/roomstyle.go)<br>[service.go:762](../../internal/application/service.go) |
| room.style.decide | executable | internal_write | `room style decide` | `/rooms` | [roomstyle.go:712](../../internal/application/roomstyle.go)<br>[service.go:762](../../internal/application/service.go) |
| room.style.preview | executable | external_read | `room style preview` | `/rooms` | [roomstyle.go:710](../../internal/application/roomstyle.go)<br>[service.go:762](../../internal/application/service.go) |
| room.style.show | executable | read | `room style show` | `/rooms` | [roomstyle.go:699](../../internal/application/roomstyle.go)<br>[service.go:762](../../internal/application/service.go) |
| runtime.autostart | executable | read | `autostart status` | `/maintenance` | [service.go:629](../../internal/application/service.go)<br>[service.go:782](../../internal/application/service.go) |
| runtime.autostart.set | executable | internal_write | `autostart set` | `/maintenance` | [service.go:553](../../internal/application/service.go)<br>[service.go:629](../../internal/application/service.go)<br>[service.go:782](../../internal/application/service.go) |
| runtime.doctor | executable | read | `doctor` | `/logs` | [service.go:650](../../internal/application/service.go) |
| runtime.drain | executable | internal_write | `runtime drain` | `/maintenance` | [service.go:799](../../internal/application/service.go) |
| runtime.history | executable | read | `runtime history` | `/logs` | [service.go:794](../../internal/application/service.go) |
| runtime.logs | executable | read | `runtime logs` | `/logs` | [service.go:821](../../internal/application/service.go) |
| runtime.pause | executable | internal_write | `runtime pause` | `/` | [admin.go:323](../../internal/application/admin.go)<br>[service.go:772](../../internal/application/service.go) |
| runtime.restart | executable | internal_write | `runtime restart` | `/maintenance` | [admin.go:325](../../internal/application/admin.go)<br>[service.go:552](../../internal/application/service.go)<br>[service.go:627](../../internal/application/service.go)<br>[service.go:772](../../internal/application/service.go) |
| runtime.status | executable | read | `runtime status` | `/` | [admin.go:323](../../internal/application/admin.go)<br>[service.go:772](../../internal/application/service.go) |
| runtime.stop | executable | internal_write | `runtime stop` | `/maintenance` | [admin.go:325](../../internal/application/admin.go)<br>[service.go:552](../../internal/application/service.go)<br>[service.go:627](../../internal/application/service.go)<br>[service.go:772](../../internal/application/service.go) |
| schedules.list | executable | read | `schedules list` | `/jobs` | [service.go:778](../../internal/application/service.go) |
| schedules.run | executable | internal_write | `schedules run` | `/jobs` | [schedule.go:189](../../internal/application/schedule.go)<br>[service.go:778](../../internal/application/service.go) |
| search.web | executable | external_read | `search` | `/tools` | [service.go:688](../../internal/application/service.go)<br>[tools.go:567](../../internal/application/tools.go) |
| serverlog.test | executable | external_write | `serverlog test` | `/rooms` | [service.go:554](../../internal/application/service.go)<br>[service.go:786](../../internal/application/service.go) |
| setup.resume | executable | internal_write | `setup` | `/setup` | [service.go:553](../../internal/application/service.go)<br>[service.go:648](../../internal/application/service.go) |
| stats.query | executable | read | `stats query` | `/stats` | [service.go:744](../../internal/application/service.go) |
| stats.series | executable | read | `stats series` | `/stats` | [admin.go:339](../../internal/application/admin.go)<br>[service.go:773](../../internal/application/service.go) |
| storage.migrate | deferred (disabled_by_operator) | none | `storage migrate` | `/maintenance` | [service.go:552](../../internal/application/service.go)<br>[service.go:770](../../internal/application/service.go)<br>[storage_admin.go:29](../../internal/application/storage_admin.go) |
| storage.status | executable | read | `storage status` | `/maintenance` | [service.go:770](../../internal/application/service.go)<br>[storage_admin.go:27](../../internal/application/storage_admin.go) |
| summaries.channel | executable | external_read | `summaries` | `/rooms` | [service.go:724](../../internal/application/service.go) |
| tools.configure | executable | internal_write | `tools configure` | `/tools` | [agent_backup.go:21](../../internal/application/agent_backup.go)<br>[service.go:742](../../internal/application/service.go) |
| tools.list | executable | read | `tools list` | `/tools` | [agent_backup.go:17](../../internal/application/agent_backup.go)<br>[service.go:742](../../internal/application/service.go) |
| tools.test | executable | external_read | `tools test` | `/tools` | [agent_backup.go:19](../../internal/application/agent_backup.go)<br>[service.go:742](../../internal/application/service.go) |
| trace.show | executable | read | `trace show` | `/logs` | [agent_backup.go:55](../../internal/application/agent_backup.go)<br>[service.go:742](../../internal/application/service.go) |
| transfer.recent | executable | read | `transfer-notices` | `/tools` | [service.go:678](../../internal/application/service.go) |
| turns.get | executable | read | `turns get` | `/turns` | [admin.go:329](../../internal/application/admin.go)<br>[service.go:773](../../internal/application/service.go) |
| turns.list | executable | read | `turns list` | `/turns` | [admin.go:329](../../internal/application/admin.go)<br>[service.go:773](../../internal/application/service.go) |
| turns.mark | executable | internal_write | `turns mark` | `/turns` | [admin.go:331](../../internal/application/admin.go)<br>[service.go:773](../../internal/application/service.go) |
| turns.marks | executable | read | `turns marks` | `/turns` | [admin.go:335](../../internal/application/admin.go)<br>[service.go:773](../../internal/application/service.go) |
| turns.sample | executable | read | `turns sample` | `/turns` | [admin.go:333](../../internal/application/admin.go)<br>[service.go:773](../../internal/application/service.go) |
| updates.apply | executable | internal_write | `updates apply` | `/maintenance` | [service.go:554](../../internal/application/service.go)<br>[service.go:792](../../internal/application/service.go) |
| updates.check | executable | read | `updates check` | `/maintenance` | [service.go:792](../../internal/application/service.go) |
| updates.status | executable | read | `updates status` | `/maintenance` | [service.go:792](../../internal/application/service.go) |
| usage.budgets | executable | read | `usage` | `/usage` | [service.go:764](../../internal/application/service.go) |
| usage.period | executable | read | `usage period` | `/usage` | [service.go:766](../../internal/application/service.go) |
| usage.reset | executable | internal_write | `usage reset` | `/usage` | [service.go:768](../../internal/application/service.go) |
| users.list | executable | read | `users` | `/people` | [service.go:752](../../internal/application/service.go) |
| weather.query | executable | external_read | `weather` | `/tools` | [service.go:666](../../internal/application/service.go) |
| web.read | executable | external_read | `read` | `/tools` | [service.go:690](../../internal/application/service.go) |
