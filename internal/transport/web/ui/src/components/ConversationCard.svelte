<script lang="ts">
  import { t, has } from '../lib/i18n.svelte'
  import { resource } from '../lib/resource.svelte'
  import { cfg, patchConfig } from '../lib/config.svelte'
  import { relative, num } from '../lib/format'
  import Card from './Card.svelte'
  import Field from './Field.svelte'
  import Select from './Select.svelte'
  import Input from './Input.svelte'
  import Status from './Status.svelte'
  import Skeleton from './Skeleton.svelte'
  import LoadState from './LoadState.svelte'

  // How a conversation with the bot starts and continues in this room (plan §5.1–5.3, 5.6, 5.7): what a
  // bare name means, which lines a reply sees, tag-free follow-ups, threads and the reply shape. Each
  // control saves on change, like the per-room tool switch. The first choice of every list is the default,
  // which is stored as "unset" so it keeps following the product default.
  let { guild, channel }: { guild: string; channel: string } = $props()
  const pol = $derived(cfg.doc?.guilds?.[guild]?.channels?.[channel] ?? {})
  const conv = $derived(pol.conversation ?? {})
  const base = $derived(['guilds', guild, 'channels', channel])

  const LISTS = {
    name_call: ['addressed', 'always', 'off'],
    attribution: ['on', 'structural', 'off'],
    follow: ['off', 'reply_only', 'window'],
    thread: ['offer', 'auto_task', 'off'],
    thread_follow: ['participants', 'all'],
    thread_recap: ['plan', 'always', 'off'],
    reply_shape: ['bubbles', 'trim', 'plain'],
    banter: ['off', 'shadow', 'on'],
  } as const
  type Key = keyof typeof LISTS
  const top = (k: Key) => (k === 'name_call' || k === 'attribution')
  const current = (k: Key): string => (top(k) ? pol[k] : conv[k]) || LISTS[k][0]
  const options = (k: Key) => LISTS[k].map((v) => ({ value: v, label: t(`conv.${k}.${v}`) }))
  async function choose(k: Key, v: string) {
    const path = top(k) ? [...base, k] : [...base, 'conversation', k]
    const isDefault = v === LISTS[k][0]
    if (isDefault && !(top(k) ? pol[k] : conv[k])) return // already unset: nothing to send
    await patchConfig(isDefault ? [{ path, op: 'unset' }] : [{ path, value: v }], { success: t('conv.saved') })
    void log.refresh()
  }
  const NUMS = {
    follow_window_sec: { def: 90, min: 10, max: 600 },
    follow_max_untagged: { def: 12, min: 1, max: 100 },
    thread_idle_close_min: { def: 30, min: 1, max: 1440 },
  } as const
  type NumKey = keyof typeof NUMS
  async function setNum(k: NumKey, raw: string) {
    const { def, min, max } = NUMS[k]
    const n = Math.round(Number(raw))
    const path = [...base, 'conversation', k]
    if (!raw.trim() || !Number.isFinite(n) || n === def) {
      if (!conv[k]) return // already unset: nothing to send
      return patchConfig([{ path, op: 'unset' }], { success: t('conv.saved') })
    }
    await patchConfig([{ path, value: Math.min(max, Math.max(min, n)) }], { success: t('conv.saved') })
  }

  const KINDS = ['name_call', 'follow', 'thread', 'backoff', 'banter']
  const log = resource<any>('conversation.log', () => ({ guild_id: guild, channel_id: channel, days: 7, limit: 8 }), { live: ['config', 'turn'] })
  // a decision code as a person reads it; "would_x" is recorded only: the feature is off, or in a mode that
  // does not act on it (follow reply_only, banter record only)
  function decisionLabel(kind: string, d: string) {
    const would = d.startsWith('would_')
    const code = would ? d.slice(6) : d
    const text = has(`conv.d.${kind}.${code}`) ? t(`conv.d.${kind}.${code}`) : code
    return would ? t('conv.would', { what: text }) : text
  }
  const outcomeLabel = (o: string) => (has(`conv.o.${o}`) ? t(`conv.o.${o}`) : o)
  const kindCounts = (kind: string) => Object.entries((log.data?.counts?.[kind] ?? {}) as Record<string, number>).sort((a, b) => b[1] - a[1])
  const kindOutcomes = (kind: string) => Object.entries((log.data?.outcomes?.[kind] ?? {}) as Record<string, number>).sort((a, b) => b[1] - a[1])
  const empty = $derived(log.data && KINDS.every((k) => kindCounts(k).length === 0))
</script>

<Card title={t('conv.title')} description={t('conv.desc')}>
  <div class="stack">
    {#each ['name_call', 'attribution'] as k (k)}
      <Field id="conv-{k}" label={t(`conv.${k}`)} hint={t(`conv.${k}.hint`)}>
        {#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} value={current(k as Key)} onchange={(e) => choose(k as Key, e.currentTarget.value)} options={options(k as Key)} />{/snippet}
      </Field>
    {/each}

    <div class="group">
      <div class="gh"><h3 class="small">{t('conv.follow')}</h3><Status tone="warn" label={t('conv.experimental')} size="sm" /></div>
      <Field id="conv-follow" label={t('conv.follow.label')} hint={t('conv.follow.hint')}>
        {#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} value={current('follow')} onchange={(e) => choose('follow', e.currentTarget.value)} options={options('follow')} />{/snippet}
      </Field>
      {#if current('follow') === 'window'}
        <Field id="conv-window" label={t('conv.follow_window_sec')} hint={t('conv.follow_window_sec.hint')}>
          {#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} type="number" inputmode="numeric" min="10" max="600" placeholder="90" value={conv.follow_window_sec || ''} onchange={(e) => setNum('follow_window_sec', e.currentTarget.value)} />{/snippet}
        </Field>
        <Field id="conv-untagged" label={t('conv.follow_max_untagged')} hint={t('conv.follow_max_untagged.hint')}>
          {#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} type="number" inputmode="numeric" min="1" max="100" placeholder="12" value={conv.follow_max_untagged || ''} onchange={(e) => setNum('follow_max_untagged', e.currentTarget.value)} />{/snippet}
        </Field>
      {/if}
    </div>

    <div class="group">
      <div class="gh"><h3 class="small">{t('conv.threads')}</h3>{#if current('thread') === 'auto_task'}<Status tone="warn" label={t('conv.experimental')} size="sm" />{/if}</div>
      <Field id="conv-thread" label={t('conv.thread.label')} hint={t('conv.thread.hint')}>
        {#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} value={current('thread')} onchange={(e) => choose('thread', e.currentTarget.value)} options={options('thread')} />{/snippet}
      </Field>
      {#if current('thread') !== 'off'}
        <Field id="conv-thread-follow" label={t('conv.thread_follow')} hint={t('conv.thread_follow.hint')}>
          {#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} value={current('thread_follow')} onchange={(e) => choose('thread_follow', e.currentTarget.value)} options={options('thread_follow')} />{/snippet}
        </Field>
        <Field id="conv-idle" label={t('conv.thread_idle_close_min')} hint={t('conv.thread_idle_close_min.hint')}>
          {#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} type="number" inputmode="numeric" min="1" max="1440" placeholder="30" value={conv.thread_idle_close_min || ''} onchange={(e) => setNum('thread_idle_close_min', e.currentTarget.value)} />{/snippet}
        </Field>
        <Field id="conv-recap" label={t('conv.thread_recap')} hint={t('conv.thread_recap.hint')}>
          {#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} value={current('thread_recap')} onchange={(e) => choose('thread_recap', e.currentTarget.value)} options={options('thread_recap')} />{/snippet}
        </Field>
      {/if}
    </div>

    <Field id="conv-shape" label={t('conv.reply_shape')} hint={t('conv.reply_shape.hint')}>
      {#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} value={current('reply_shape')} onchange={(e) => choose('reply_shape', e.currentTarget.value)} options={options('reply_shape')} />{/snippet}
    </Field>

    <div class="gh"><h3 class="small">{t('conv.banter_title')}</h3><Status tone="warn" label={t('conv.experimental')} size="sm" /></div>
    <Field id="conv-banter" label={t('conv.banter')} hint={t('conv.banter.hint')}>
      {#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} value={current('banter')} onchange={(e) => choose('banter', e.currentTarget.value)} options={options('banter')} />{/snippet}
    </Field>

    <section class="summary" aria-labelledby="conv-log-h">
      <h3 class="small" id="conv-log-h">{t('conv.log_title', { days: log.data?.days ?? 7 })}</h3>
      <p class="xs muted">{t('conv.log_hint')}</p>
      {#if !log.data}<LoadState res={log}><Skeleton lines={3} /></LoadState>
      {:else if empty}<p class="small muted">{t('conv.log_empty')}</p>
      {:else}
        <dl class="kinds">
          {#each KINDS.filter((k) => kindCounts(k).length) as k (k)}
            <div>
              <dt class="small">{t(`conv.kind.${k}`)}</dt>
              <dd class="row small">
                {#each kindCounts(k) as [d, n] (d)}<span class="chip">{decisionLabel(k, d)} {num(n)}</span>{/each}
                {#each kindOutcomes(k) as [o, n] (o)}<span class="chip accent" title={t('conv.outcome_hint')}>{outcomeLabel(o)} {num(n)}</span>{/each}
              </dd>
            </div>
          {/each}
        </dl>
        {#if (log.data.rows ?? []).length}
          <ul class="rows">
            {#each log.data.rows as r (r.id)}
              <li class="xs"><span class="muted num nowrap">{relative(r.at)}</span> <span>{t(`conv.kind.${r.kind}`)}</span> · <span>{decisionLabel(r.kind, r.decision)}</span>{#if r.score} <span class="muted num">{Number(r.score).toFixed(2)}</span>{/if}{#if r.outcome} · <span class="muted">{outcomeLabel(r.outcome)}</span>{/if}</li>
            {/each}
          </ul>
        {/if}
      {/if}
    </section>
  </div>
</Card>

<style>
  .group {
    display: grid;
    gap: var(--s-3);
    padding-block-start: var(--s-3);
    border-block-start: 1px solid var(--border);
  }
  .gh {
    display: flex;
    align-items: center;
    gap: var(--s-2);
    flex-wrap: wrap;
  }
  .gh h3,
  .summary h3 {
    margin: 0;
  }
  .summary {
    display: grid;
    gap: var(--s-2);
    padding-block-start: var(--s-3);
    border-block-start: 1px solid var(--border);
  }
  .summary p {
    margin: 0;
  }
  .kinds {
    display: grid;
    gap: var(--s-2);
    margin: 0;
  }
  .kinds dt {
    font-weight: 550;
  }
  .kinds dd {
    margin: 0;
    flex-wrap: wrap;
    gap: 6px;
  }
  .rows {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 2px;
    color: var(--text-muted);
  }
</style>
