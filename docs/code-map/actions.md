# Action 색인

internal/catalog의 모든 Action: CLI 명령, 콘솔 경로와 internal/application에서의 처리 함수.

이 파일은 생성됩니다: go run ./tools/codemap (손으로 고치지 마세요)

원본: `internal/catalog/catalog.go`의 `catalog.All()` (ID·상태·효과·CLI·경로), `internal/application`의 `switch req.Action`/`if req.Action ==` 분기 (처리: 본문이 요청을 넘기는 한 호출뿐인 순수 중계를 따라간 도착 함수, 아니면 그 분기를 품은 함수) — go/parser로 읽은 위치는 줄 번호를 쓰지 않아 관련 없는 편집에 지도가 낡지 않음

| Action | 상태 | 효과 | CLI 명령 | 콘솔 경로 | 처리 |
|---|---|---|---|---|---|
| access.policy | executable | internal_write | `access` | `/people` | — |
| activity.ranking | executable | read | `activity ranking` | `/tools` | — |
| admin.prefs | executable | read | `prefs show` | `/` | [admin.go · adminPrefs](../../internal/application/admin.go) |
| admin.prefs.set | executable | internal_write | `prefs set` | `/` | [admin.go · adminPrefs](../../internal/application/admin.go) |
| agent.simulate | executable | read | `agent simulate` | `/lab` | [agent_backup.go · agent](../../internal/application/agent_backup.go) |
| alerts.status | executable | read | `alerts status` | `/maintenance` | — |
| alerts.test | executable | external_write | `alerts test` | `/maintenance` | — |
| audit.list | executable | read | `audit` | `/settings` | [admin.go · auditList](../../internal/application/admin.go) |
| backup.create | executable | internal_write | `backup create` | `/maintenance` | [backup.go · backup](../../internal/application/backup.go) |
| backup.delete | executable | internal_write | `backup delete` | `/maintenance` | [backup.go · backup](../../internal/application/backup.go) |
| backup.list | executable | read | `backup list` | `/maintenance` | [backup.go · backup](../../internal/application/backup.go) |
| backup.restore | executable | internal_write | `backup restore` | `/maintenance` | [backup.go · backup](../../internal/application/backup.go) |
| backup.restoreDryRun | executable | read | `backup restore-preview` | `/maintenance` | [backup.go · backup](../../internal/application/backup.go) |
| backup.verify | executable | read | `backup verify` | `/maintenance` | [backup.go · backup](../../internal/application/backup.go) |
| channels.attachments.add | executable | internal_write | `channels attachments add` | `/rooms` | [channels.go · channelsAttachmentAdd](../../internal/application/channels.go) |
| channels.attachments.remove | executable | internal_write | `channels attachments remove` | `/rooms` | [channels.go · channelsAttachmentRemove](../../internal/application/channels.go) |
| channels.configure | executable | internal_write | `channels configure` | `/rooms` | [channels.go · channelsConfigure](../../internal/application/channels.go) |
| channels.invite | executable | read | `channels invite` | `/connections` | [channels.go · channelsInvite](../../internal/application/channels.go) |
| channels.leave | executable | external_write | `channels leave` | `/rooms` | [channels.go · channelsLeave](../../internal/application/channels.go) |
| channels.list | executable | read | `channels list` | `/rooms` | [channels.go · channelsList](../../internal/application/channels.go) |
| channels.ops_log | executable | read | `channels ops-log` | `/rooms` | [channels.go · channelsOpsLog](../../internal/application/channels.go) |
| channels.permissions | executable | read | `channels permissions` | `/rooms` | — |
| channels.view.attach | executable | internal_write | `channels view attach` | `/rooms` | [channels.go · channelsViewAttach](../../internal/application/channels.go) |
| channels.view.detach | executable | internal_write | `channels view detach` | `/rooms` | [channels.go · channelsViewDetach](../../internal/application/channels.go) |
| channels.view.show | executable | read | `channels view show` | `/rooms` | [channels.go · channelsViewShow](../../internal/application/channels.go) |
| chat.respond | executable | external_read | `chat` | `/lab` | [memory.go · memoryActions](../../internal/application/memory.go) |
| config.apply | executable | internal_write | `config apply` | `/settings` | — |
| config.changes | executable | read | `config changes` | `/settings` | [config_actions.go · configChangesAction](../../internal/application/config_actions.go) |
| config.diff | executable | read | `config diff` | `/settings` | — |
| config.document | executable | read | `config document` | `/settings` | [admin.go · configDocument](../../internal/application/admin.go) |
| config.patch | executable | internal_write | `config patch` | `/settings` | [admin.go · configPatch](../../internal/application/admin.go) |
| config.rollback | executable | internal_write | `config rollback` | `/settings` | — |
| config.show | executable | read | `config show` | `/settings` | — |
| config.snapshot | executable | internal_write | `config snapshot save` | `/settings` | [ops_extra.go · snapshotsAction](../../internal/application/ops_extra.go) |
| config.snapshot.delete | executable | internal_write | `config snapshot delete` | `/settings` | [ops_extra.go · snapshotsAction](../../internal/application/ops_extra.go) |
| config.snapshot.restore | executable | internal_write | `config snapshot restore` | `/settings` | [ops_extra.go · snapshotsAction](../../internal/application/ops_extra.go) |
| config.snapshots | executable | read | `config snapshots` | `/settings` | [ops_extra.go · snapshotsAction](../../internal/application/ops_extra.go) |
| config.validate | executable | read | `config validate` | `/settings` | — |
| conversation.log | executable | read | `conversation log` | `/rooms` | — |
| dashboard.overview | executable | read | `dashboard` | `/` | [admin.go · dashboardOverview](../../internal/application/admin.go) |
| disasters.alerts | executable | read | `disasters alerts` | `/tools` | — |
| disasters.query | executable | external_read | `disasters` | `/tools` | — |
| discord.check | executable | internal_write | `discord check` | `/connections` | — |
| eval.run | executable | external_read | `eval run` | `/lab` | — |
| eval.runs | executable | read | `eval runs` | `/lab` | — |
| events.timeline | executable | read | `events` | `/memory` | — |
| feedback.summary | executable | read | `feedback summary` | `/turns` | — |
| finance.fx | executable | external_read | `finance fx` | `/tools` | — |
| finance.quote | executable | external_read | `finance quote` | `/tools` | — |
| fortune.query | executable | external_read | `fortune` | `/tools` | — |
| fortune.subscribe | executable | internal_write | `fortune subscribe` | `/people` | — |
| help.version | executable | read | `help` | `/` | — |
| images.generate | executable | external_write | `images` | `/tools` | — |
| imports.preview | executable | read | `imports preview` | `/blocks` | — |
| imports.start | executable | internal_write | `imports start` | `/blocks` | [memory.go · memoryActions](../../internal/application/memory.go) |
| instance.reset | executable | internal_write | `reset` | `/maintenance` | — |
| jobs.cancel | executable | internal_write | `jobs cancel` | `/jobs` | [jobs_actions.go · jobsAction](../../internal/application/jobs_actions.go) |
| jobs.get | executable | read | `jobs get` | `/jobs` | [jobs_actions.go · jobsAction](../../internal/application/jobs_actions.go) |
| jobs.list | executable | read | `jobs list` | `/jobs` | [jobs_actions.go · jobsAction](../../internal/application/jobs_actions.go) |
| jobs.retry | executable | internal_write | `jobs retry` | `/jobs` | [jobs_actions.go · jobsAction](../../internal/application/jobs_actions.go) |
| jobs.watch | executable | read | `jobs watch` | `/jobs` | [jobs_actions.go · jobsAction](../../internal/application/jobs_actions.go) |
| lab.review | executable | internal_write | `lab review` | `/lab` | [admin.go · labReviews](../../internal/application/admin.go) |
| lab.reviews | executable | read | `lab reviews` | `/lab` | [admin.go · labReviews](../../internal/application/admin.go) |
| locales.list | executable | read | `locales` | `/persona` | — |
| memory.backfill | executable | internal_write | `memory backfill` | `/rooms` | — |
| memory.blocks.delete | executable | internal_write | `memory blocks delete` | `/blocks` | [memory_blocks.go · deleteMemoryBlock](../../internal/application/memory_blocks.go) |
| memory.blocks.list | executable | read | `memory blocks list` | `/blocks` | [memory_blocks.go · memoryBlocks](../../internal/application/memory_blocks.go) |
| memory.blocks.prepare | executable | external_write | `memory blocks prepare` | `/blocks` | [memory_blocks.go · prepareMemoryBlock](../../internal/application/memory_blocks.go) |
| memory.blocks.show | executable | read | `memory blocks show` | `/blocks` | [memory_blocks.go · memoryBlocks](../../internal/application/memory_blocks.go) |
| memory.blocks.update | executable | internal_write | `memory blocks update` | `/blocks` | [memory_blocks.go · updateMemoryBlock](../../internal/application/memory_blocks.go) |
| memory.episodes.extract | executable | external_write | `memory episodes extract` | `/blocks` | [episodes_actions.go · episodesExtract](../../internal/application/episodes_actions.go) |
| memory.episodes.forget | executable | internal_write | `memory episodes forget` | `/memory` | [episodes_actions.go · episodesForget](../../internal/application/episodes_actions.go) |
| memory.episodes.list | executable | read | `memory episodes list` | `/memory` | [episodes_actions.go · episodesList](../../internal/application/episodes_actions.go) |
| memory.episodes.show | executable | read | `memory episodes show` | `/memory` | [episodes_actions.go · episodeShow](../../internal/application/episodes_actions.go) |
| memory.mine.forget | executable | internal_write | `memory mine forget` | `/memory` | [explicit.go · mineAction](../../internal/application/explicit.go) |
| memory.mine.show | executable | read | `memory mine show` | `/memory` | [explicit.go · mineAction](../../internal/application/explicit.go) |
| memory.reindex | executable | external_write | `memory reindex` | `/blocks` | [memory.go · memoryActions](../../internal/application/memory.go) |
| memory.review | executable | internal_write | `memory review` | `/memory` | — |
| memory.reviews | executable | read | `memory reviews` | `/memory` | — |
| memory.search | executable | read | `memory search` | `/memory` | [memory.go · memoryActions](../../internal/application/memory.go) |
| memory.state.show | executable | read | `memory state show` | `/memory` | [episodes_actions.go · stateShow](../../internal/application/episodes_actions.go) |
| messages.search | executable | read | `messages search` | `/memory` | [memory.go · memoryActions](../../internal/application/memory.go) |
| migration.apply | executable | internal_write | `migration apply` | `/maintenance` | — |
| models.embed_width | executable | internal_write | `models embed-width` | `/connections` | [embed_width.go · modelsEmbedWidth](../../internal/application/embed_width.go) |
| models.list | executable | read | `models list` | `/connections` | [providers_persona.go · providers](../../internal/application/providers_persona.go) |
| models.recommend | executable | internal_write | `models recommend` | `/connections` | [providers_persona.go · modelsRecommend](../../internal/application/providers_persona.go) |
| models.select | executable | internal_write | `models select` | `/connections` | [providers_persona.go · providers](../../internal/application/providers_persona.go) |
| notices.status | executable | read | `notices status` | `/tools` | — |
| notices.subscribe | executable | internal_write | `notices subscribe` | `/people` | — |
| participation.mode | executable | internal_write | `participation` | `/rooms` | — |
| people.list | executable | read | `people list` | `/memory` | — |
| people.merge | executable | internal_write | `people merge` | `/memory` | — |
| people.servers | executable | read | `people servers` | `/people` | — |
| personas.apply | executable | internal_write | `personas apply` | `/persona` | [channels.go · personas](../../internal/application/channels.go) |
| personas.list | executable | read | `personas list` | `/persona` | [channels.go · personas](../../internal/application/channels.go) |
| places.search | executable | external_read | `places search` | `/tools` | — |
| polls.close | executable | internal_write | `polls close` | `/tools` | [features.go · polls](../../internal/application/features.go) |
| polls.create | executable | internal_write | `polls create` | `/tools` | [features.go · polls](../../internal/application/features.go) |
| polls.result | executable | read | `polls result` | `/tools` | [features.go · polls](../../internal/application/features.go) |
| polls.vote | executable | internal_write | `polls vote` | `/tools` | [features.go · polls](../../internal/application/features.go) |
| preferences.set | executable | internal_write | `preferences` | `/people` | — |
| privacy.consent | executable | internal_write | `privacy` | `/people` | — |
| providers.add | executable | internal_write | `providers add` | `/connections` | [providers_persona.go · providerAdd](../../internal/application/providers_persona.go) |
| providers.credentials | executable | internal_write | `providers credentials` | `/connections` | [providers_persona.go · providers](../../internal/application/providers_persona.go) |
| providers.list | executable | read | `providers list` | `/connections` | [providers_persona.go · providers](../../internal/application/providers_persona.go) |
| providers.remove | executable | internal_write | `providers remove` | `/connections` | [providers_persona.go · providers](../../internal/application/providers_persona.go) |
| providers.test | executable | read | `providers test` | `/connections` | [providers_persona.go · providers](../../internal/application/providers_persona.go) |
| reminders.propose | executable | internal_write | `reminders propose` | `/rooms` | — |
| reminders.update | executable | internal_write | `reminders` | `/rooms` | — |
| reports.create | executable | read | `reports create` | `/stats` | — |
| room.style.build | executable | external_write | `room style build` | `/rooms` | [roomstyle.go · roomStyleAction](../../internal/application/roomstyle.go) |
| room.style.decide | executable | internal_write | `room style decide` | `/rooms` | [roomstyle.go · roomStyleAction](../../internal/application/roomstyle.go) |
| room.style.preview | executable | external_read | `room style preview` | `/rooms` | [roomstyle.go · roomStylePreview](../../internal/application/roomstyle.go) |
| room.style.show | executable | read | `room style show` | `/rooms` | [roomstyle.go · roomStyleShow](../../internal/application/roomstyle.go) |
| runtime.autostart | executable | read | `autostart status` | `/maintenance` | [schedule.go · autostartAction](../../internal/application/schedule.go) |
| runtime.autostart.set | executable | internal_write | `autostart set` | `/maintenance` | [schedule.go · autostartAction](../../internal/application/schedule.go) |
| runtime.doctor | executable | read | `doctor` | `/logs` | — |
| runtime.drain | executable | internal_write | `runtime drain` | `/maintenance` | — |
| runtime.history | executable | read | `runtime history` | `/logs` | — |
| runtime.logs | executable | read | `runtime logs` | `/logs` | — |
| runtime.pause | executable | internal_write | `runtime pause` | `/` | [admin.go · runtimePause](../../internal/application/admin.go) |
| runtime.restart | executable | internal_write | `runtime restart` | `/maintenance` | [admin.go · runtimeRestart](../../internal/application/admin.go) |
| runtime.status | executable | read | `runtime status` | `/` | [admin.go · runtimePause](../../internal/application/admin.go) |
| runtime.stop | executable | internal_write | `runtime stop` | `/maintenance` | [admin.go · runtimeRestart](../../internal/application/admin.go) |
| schedules.list | executable | read | `schedules list` | `/jobs` | — |
| schedules.run | executable | internal_write | `schedules run` | `/jobs` | [schedule.go · schedulesAction](../../internal/application/schedule.go) |
| search.web | executable | external_read | `search` | `/tools` | — |
| serverlog.test | executable | external_write | `serverlog test` | `/rooms` | — |
| setup.resume | executable | internal_write | `setup` | `/setup` | — |
| stats.export | executable | read | `stats export` | `/stats` | — |
| stats.query | executable | read | `stats query` | `/stats` | — |
| stats.rollup | executable | internal_write | `stats rollup` | `/jobs` | — |
| stats.series | executable | read | `stats series` | `/stats` | [admin.go · statsSeries](../../internal/application/admin.go) |
| storage.migrate | deferred (disabled_by_operator) | none | `storage migrate` | `/maintenance` | [storage_admin.go · storageMigrate](../../internal/application/storage_admin.go) |
| storage.status | executable | read | `storage status` | `/maintenance` | [storage_admin.go · storageStatus](../../internal/application/storage_admin.go) |
| summaries.channel | executable | external_read | `summaries` | `/rooms` | — |
| tools.configure | executable | internal_write | `tools configure` | `/tools` | [tools.go · toolsConfigure](../../internal/application/tools.go) |
| tools.list | executable | read | `tools list` | `/tools` | [tools.go · toolsList](../../internal/application/tools.go) |
| tools.test | executable | external_read | `tools test` | `/tools` | [tools.go · toolsTest](../../internal/application/tools.go) |
| trace.show | executable | read | `trace show` | `/logs` | [agent_backup.go · agent](../../internal/application/agent_backup.go) |
| transfer.recent | executable | read | `transfer-notices` | `/tools` | — |
| turns.get | executable | read | `turns get` | `/turns` | [admin.go · turnsAction](../../internal/application/admin.go) |
| turns.list | executable | read | `turns list` | `/turns` | [admin.go · turnsAction](../../internal/application/admin.go) |
| turns.mark | executable | internal_write | `turns mark` | `/turns` | [operator_marks.go · turnMarkAction](../../internal/application/operator_marks.go) |
| turns.marks | executable | read | `turns marks` | `/turns` | [operator_marks.go · turnMarksAction](../../internal/application/operator_marks.go) |
| turns.sample | executable | read | `turns sample` | `/turns` | [operator_marks.go · turnSampleAction](../../internal/application/operator_marks.go) |
| updates.apply | executable | internal_write | `updates apply` | `/maintenance` | — |
| updates.check | executable | read | `updates check` | `/maintenance` | — |
| updates.notes | executable | read | `updates notes` | `/maintenance` | — |
| updates.status | executable | read | `updates status` | `/maintenance` | — |
| updates.subscribe | executable | internal_write | `updates subscribe` | `/people` | — |
| usage.budgets | executable | read | `usage` | `/usage` | — |
| usage.period | executable | read | `usage period` | `/usage` | — |
| usage.reset | executable | internal_write | `usage reset` | `/usage` | — |
| users.list | executable | read | `users` | `/people` | — |
| weather.query | executable | external_read | `weather` | `/tools` | — |
| web.read | executable | external_read | `read` | `/tools` | — |
