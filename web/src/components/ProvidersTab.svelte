<script lang="ts">
  import { onMount } from 'svelte'
  import QwenConnection from './QwenConnection.svelte'
  import { tierColor } from '../lib/tiers'
  import type { ProviderStatus } from '../stores/requests'

  let providers: ProviderStatus[] = []
  let loading = true
  let refreshing = false
  let error = ''

  async function load(isRefresh = false) {
    if (isRefresh) refreshing = true
    error = ''
    try {
      const res = await fetch('/api/providers', { cache: 'no-store' })
      if (!res.ok) throw new Error(`the dashboard API returned ${res.status}`)
      const data = await res.json()
      providers = (data.providers ?? []) as ProviderStatus[]
    } catch (err) {
      error = err instanceof Error ? err.message : 'could not reach the dashboard API'
    } finally {
      loading = false
      refreshing = false
    }
  }

  onMount(() => load())

  // The Qwen subscription is a provider card even before it exists in config,
  // so it is rendered apart from — and ahead of — the configured rows.
  $: qwenEntry = providers.find(p => p.name === 'qwen-subscription')
  $: rows = providers.filter(p => p.name !== 'qwen-subscription')

  const DISCOVERY_NOTE: Record<string, string> = {
    pending: 'Model discovery is pending — showing the fallback list.',
    unsupported: 'This provider exposes no model listing — showing its configured models.',
    failed: 'Model listing is failing — showing the last known models.',
    off: 'Model discovery is disabled ([catalog] enabled = false).',
    not_loaded: 'Not loaded by the running proxy — restart NEXUS to activate it.',
  }

  function keySummary(p: ProviderStatus): string {
    if (!p.keys) return 'no key'
    const n = `${p.keys} ${p.keys === 1 ? 'key' : 'keys'}`
    if (p.key_source === 'env') return p.key_env ? `${n} · from ${p.key_env}` : `${n} · from environment`
    return n
  }
</script>

<!-- Providers tab: one card per configured provider plus the Qwen subscription
     card, which is always present. Each card names the provider, its tier and
     credentials, and the models it serves with an honest source label. -->
<div class="pt" role="tabpanel" id="panel-providers" aria-labelledby="tab-providers">
  <div class="pt-head">
    <div class="pt-title">Model providers</div>
    <button type="button" class="pt-refresh" on:click={() => load(true)} disabled={refreshing || loading}
            aria-label="Refresh provider list">{refreshing ? 'refreshing…' : '↻ refresh'}</button>
  </div>

  {#if loading}
    <div class="pt-state" role="status">Loading providers…</div>
  {:else if error}
    <div class="pt-state err" role="alert">
      <p>Could not load providers: {error}.</p>
      <button type="button" class="pt-retry" on:click={() => load()}>Retry</button>
    </div>
  {:else}
    <QwenConnection entry={qwenEntry} />

    {#if rows.length === 0}
      <div class="pt-empty">
        {#if qwenEntry}No other providers configured.{:else}No providers configured yet.{/if}
        Add one with <code>nexus add groq &lt;api-key&gt;</code> or edit <code>~/.nexus/config.toml</code>, then restart NEXUS.
      </div>
    {/if}

    {#each rows as p (p.name)}
      <article class="p-card" aria-label="{p.name} provider">
        <div class="p-head">
          <div class="p-id">
            <span class="p-name">{p.name}</span>
            {#if p.family && p.family !== p.name}
              <span class="p-family">{p.family}</span>
            {/if}
          </div>
          <span class="p-tier" style="color:{tierColor(p.tier)}">{p.tier}</span>
        </div>

        <div class="p-meta">
          <span>{keySummary(p)}</span>
          {#if p.base_url}<span class="p-url">{p.base_url}</span>{/if}
        </div>

        <div class="p-models">
          <span class="p-src {p.model_source ?? 'defaults'}">{p.model_source ?? 'defaults'}</span>
          {#each p.models ?? [] as m (m)}
            <span class="p-model">{m}</span>
          {:else}
            <span class="p-none">no models known yet</span>
          {/each}
        </div>

        {#if p.discovery && p.discovery !== 'ok' && DISCOVERY_NOTE[p.discovery]}
          <p class="p-note" class:warn={p.discovery === 'not_loaded' || p.discovery === 'failed'}>{DISCOVERY_NOTE[p.discovery]}</p>
        {/if}
      </article>
    {/each}
  {/if}
</div>

<style>
  .pt { max-width: 1100px; margin: 0 auto; padding: 20px 28px 40px; display: flex; flex-direction: column; gap: 12px; }
  .pt-head { display: flex; align-items: center; justify-content: space-between; gap: 10px; }
  .pt-title { font-size: 10px; color: #64748b; text-transform: uppercase; letter-spacing: 0.12em; }
  .pt-refresh { background: transparent; color: #94a3b8; border: 1px solid #1a2035; border-radius: 6px; padding: 5px 11px; font-size: 11px; cursor: pointer; }
  .pt-refresh:hover { color: #e2e8f0; border-color: #2a2150; }
  .pt-refresh:disabled { opacity: 0.5; cursor: default; }

  .pt-state { color: #64748b; background: #0a0e1a; border: 1px solid #1a2035; border-radius: 8px; padding: 40px; text-align: center; min-height: 120px; display: flex; flex-direction: column; gap: 12px; align-items: center; justify-content: center; }
  .pt-state.err { color: #ef4444; }
  .pt-retry { background: #7c3aed; color: #fff; border: 0; border-radius: 6px; padding: 7px 14px; font-size: 12px; font-weight: 700; cursor: pointer; }
  .pt-retry:hover { background: #6d28d9; }

  .pt-empty { color: #64748b; font-size: 12px; line-height: 1.6; background: #0a0e1a; border: 1px solid #1a2035; border-radius: 8px; padding: 14px 18px; }
  .pt-empty code { color: #06b6d4; font-family: 'Geist Mono', monospace; font-size: 11px; }

  .p-card { display: flex; flex-direction: column; gap: 8px; background: #0a0e1a; border: 1px solid #1a2035; border-radius: 8px; padding: 16px 18px; }
  .p-head { display: flex; align-items: center; justify-content: space-between; gap: 10px; flex-wrap: wrap; }
  .p-id { display: flex; align-items: baseline; gap: 8px; min-width: 0; flex-wrap: wrap; }
  .p-name { color: #06b6d4; font-weight: 600; font-size: 13px; }
  .p-family { color: #64748b; font-size: 10px; text-transform: uppercase; letter-spacing: 0.1em; }
  .p-tier { font-size: 10px; text-transform: uppercase; letter-spacing: 0.1em; font-weight: 600; }

  .p-meta { display: flex; flex-wrap: wrap; gap: 5px 14px; color: #64748b; font-size: 11px; font-family: 'Geist Mono', monospace; }
  .p-url { color: #94a3b8; word-break: break-all; }

  .p-models { display: flex; flex-wrap: wrap; gap: 5px; align-items: center; }
  .p-src { font-size: 9px; padding: 2px 8px; border-radius: 4px; text-transform: uppercase; letter-spacing: 0.08em; }
  .p-src.live { background: rgba(16,185,129,0.12); color: #10b981; }
  .p-src.configured { background: rgba(6,182,212,0.12); color: #06b6d4; }
  .p-src.defaults { background: rgba(100,116,139,0.14); color: #94a3b8; }
  .p-model { font-family: 'Geist Mono', monospace; font-size: 11px; color: #94a3b8; background: #11182f; border: 1px solid #1a2035; border-radius: 4px; padding: 2px 8px; }
  .p-none { color: #64748b; font-size: 11px; }

  .p-note { color: #64748b; font-size: 11px; line-height: 1.5; }
  .p-note.warn { color: #f59e0b; }

  @media (max-width: 620px) {
    .pt { padding: 16px 16px 32px; }
    .pt-refresh, .pt-retry { min-height: 44px; }
  }
</style>