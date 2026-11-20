<script lang="ts">
  import { onDestroy } from 'svelte'
  import type { ProviderStatus } from '../stores/requests'

  // The configured qwen-subscription entry once the proxy has loaded it —
  // supplies the discovered model ids shown on the card.
  export let entry: ProviderStatus | undefined = undefined

  type Flow = { id: string; url: string; user_code: string }
  type Status = { connected: boolean; loaded: boolean; models: number; flow?: Flow; message?: string }

  let token = ''
  let status: Status | null = null
  let busy = false
  let error = ''
  let unavailable = ''
  let connectIntent = false
  let poll: ReturnType<typeof setInterval> | undefined

  async function request<T>(path: string, method = 'GET'): Promise<T> {
    const res = await fetch('/api/qwen/' + path, {
      method, headers: { Authorization: 'Bearer ' + token.trim() }, cache: 'no-store',
    })
    if (res.status === 401) status = null
    if (res.status === 204) return undefined as T
    const result = await res.json().catch(() => null)
    if (res.status === 503) {
      // Deployment-level problem (no operator token, no session store): the
      // server's own message says what to fix, so keep it verbatim.
      unavailable = result?.error || 'Qwen sign-in is unavailable on this deployment.'
      status = null
      throw new Error(unavailable)
    }
    if (!res.ok) throw new Error(result?.error || 'Could not reach the Qwen sign-in controls.')
    return result as T
  }

  async function act(action: () => Promise<void>) {
    if (busy) return
    busy = true
    error = ''
    try {
      await action()
    } catch (err) {
      error = err instanceof Error ? err.message : 'Qwen sign-in failed.'
    } finally {
      busy = false
    }
  }

  async function unlock() {
    await act(async () => {
      status = await request<Status>('status')
      unavailable = ''
      if (!poll) {
        poll = setInterval(() => {
          if (!status) return
          void act(async () => { status = await request<Status>('status') })
        }, 2500)
      }
    })
  }

  async function start() {
    await act(async () => {
      if (!status) return
      const flow = await request<Flow>('flow', 'POST')
      status = { ...status, flow, message: undefined }
    })
  }

  async function cancel(flow: Flow) {
    await act(async () => {
      await request('flow/' + encodeURIComponent(flow.id), 'DELETE')
      status = await request<Status>('status')
    })
  }

  function primary() {
    if (!status) { connectIntent = true; return }
    void start()
  }

  function retry() {
    unavailable = ''
    error = ''
    if (token.trim()) void unlock()
    else connectIntent = true
  }

  $: badge = !status ? { text: 'unknown', cls: 'idle' }
    : status.flow ? { text: 'awaiting approval', cls: 'wait' }
    : status.connected
      ? (status.loaded ? { text: 'connected', cls: 'ok' } : { text: 'restart needed', cls: 'wait' })
      : { text: 'not connected', cls: 'idle' }

  $: statusLine = !status || !status.connected
    ? 'Sign in with a Qwen account that has a Code subscription.'
    : !status.loaded
      ? 'Signed in. Restart NEXUS to load the Qwen provider.'
      : status.models
        ? `Connected. ${status.models} Qwen ${status.models === 1 ? 'model' : 'models'} ready.`
        : 'Signed in. Qwen model discovery is pending or unavailable.'

  $: primaryLabel = !status ? 'Connect Qwen' : status.connected ? 'Reconnect Qwen' : 'Start Qwen sign-in'

  onDestroy(() => { if (poll) clearInterval(poll) })
</script>

<!-- Qwen subscription provider card: the dashboard's only Qwen sign-in surface.
     The header carries identity + live status; the body discloses the operator
     token form, the device-code flow, or the reconnect action as state requires. -->
<article class="q-card" aria-label="Qwen subscription provider">
  <div class="q-head">
    <div class="q-id">
      <span class="q-name">qwen-subscription</span>
      <span class="q-sub">Qwen Code subscription · OAuth sign-in, no API key</span>
    </div>
    <span class="q-badge {badge.cls}">{badge.text}</span>
  </div>

  {#if unavailable}
    <p class="q-notice" role="alert">{unavailable}</p>
    <button type="button" class="q-btn ghost" on:click={retry}>Try again</button>
  {:else}
    {#if status?.flow}
      <!-- Device-code flow: the user approves in their browser while NEXUS
           polls the grant in the background. -->
      <div class="q-flow">
        <p role="status">Waiting for approval in Qwen.</p>
        <p>User code <code>{status.flow.user_code}</code></p>
        <div class="q-flow-actions">
          <a href={status.flow.url} target="_blank" rel="noopener noreferrer">Approve on Qwen ↗</a>
          <button type="button" class="q-btn ghost" disabled={busy} on:click={() => status?.flow && cancel(status.flow)}>Cancel</button>
        </div>
      </div>
    {:else}
      <p class="q-status" role="status">{statusLine}</p>
      {#if connectIntent && !status}
        <!-- Operator token gate, disclosed only after the user asks to connect. -->
        <form class="q-form" on:submit|preventDefault={unlock}>
          <label>Dashboard operator token
            <input type="password" autocomplete="off" bind:value={token} required />
          </label>
          <button type="submit" class="q-btn" disabled={busy}>{busy ? 'Checking…' : 'Continue'}</button>
        </form>
      {:else}
        <button type="button" class="q-btn" disabled={busy} on:click={primary}>{primaryLabel}</button>
      {/if}
    {/if}
    {#if status?.message}<p class="q-status" role="status">{status.message}</p>{/if}
    {#if error}<p class="q-err" role="alert">{error}</p>{/if}
    {#if status?.connected && status.loaded && (entry?.models?.length ?? 0) > 0}
      <!-- Discovered ids come from the providers API entry; the status count
           alone cannot name them. -->
      <div class="q-models">
        <span class="q-src live">live</span>
        {#each entry?.models ?? [] as m (m)}
          <span class="q-model">{m}</span>
        {/each}
      </div>
    {/if}
  {/if}
</article>

<style>
  .q-card { display: flex; flex-direction: column; gap: 10px; background: #0a0e1a; border: 1px solid #1a2035; border-radius: 8px; padding: 16px 18px; }
  .q-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 10px; flex-wrap: wrap; }
  .q-id { display: flex; flex-direction: column; gap: 2px; min-width: 0; }
  .q-name { color: #06b6d4; font-weight: 600; font-size: 13px; }
  .q-sub { color: #64748b; font-size: 11px; }
  .q-badge { font-size: 9px; padding: 2px 8px; border-radius: 4px; text-transform: uppercase; letter-spacing: 0.08em; white-space: nowrap; }
  .q-badge.ok { background: rgba(16,185,129,0.12); color: #10b981; }
  .q-badge.wait { background: rgba(245,158,11,0.12); color: #f59e0b; }
  .q-badge.idle { background: rgba(100,116,139,0.14); color: #94a3b8; }

  .q-status { color: #94a3b8; font-size: 12px; line-height: 1.5; }
  .q-btn { background: #7c3aed; color: #fff; border: 0; border-radius: 6px; padding: 7px 14px; font-size: 12px; font-weight: 700; cursor: pointer; align-self: flex-start; }
  .q-btn:hover { background: #6d28d9; }
  .q-btn:disabled { opacity: 0.5; cursor: default; }
  .q-btn.ghost { background: transparent; color: #94a3b8; border: 1px solid #1a2035; font-weight: 600; }
  .q-btn.ghost:hover { color: #e2e8f0; border-color: #2a2150; background: transparent; }

  .q-form { display: flex; flex-wrap: wrap; gap: 10px; align-items: end; }
  .q-form label { display: grid; gap: 4px; color: #94a3b8; font-size: 11px; }
  .q-form input { background: #050816; color: #e2e8f0; border: 1px solid #1a2035; border-radius: 5px; padding: 7px 9px; font-size: 12px; font-family: 'Geist Mono', monospace; min-width: 220px; }
  .q-form input:focus { outline: none; border-color: #7c3aed; }

  .q-flow { display: flex; flex-direction: column; gap: 6px; background: #11182f; border: 1px solid #1a2035; border-radius: 7px; padding: 10px 12px; font-size: 12px; color: #94a3b8; }
  .q-flow p { margin: 0; }
  .q-flow code { color: #e2e8f0; font-family: 'Geist Mono', monospace; font-size: 13px; letter-spacing: 0.08em; }
  .q-flow-actions { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
  .q-flow a { color: #06b6d4; font-size: 12px; font-weight: 600; }

  .q-notice { color: #f59e0b; font-size: 12px; line-height: 1.5; background: rgba(245,158,11,0.08); border: 1px solid rgba(245,158,11,0.25); border-radius: 7px; padding: 8px 12px; }
  .q-err { color: #ef4444; font-size: 12px; background: rgba(239,68,68,0.08); border: 1px solid rgba(239,68,68,0.25); border-radius: 7px; padding: 8px 12px; }

  .q-models { display: flex; flex-wrap: wrap; gap: 5px; align-items: center; }
  .q-src { font-size: 9px; padding: 2px 8px; border-radius: 4px; text-transform: uppercase; letter-spacing: 0.08em; }
  .q-src.live { background: rgba(16,185,129,0.12); color: #10b981; }
  .q-model { font-family: 'Geist Mono', monospace; font-size: 11px; color: #94a3b8; background: #11182f; border: 1px solid #1a2035; border-radius: 4px; padding: 2px 8px; }

  @media (max-width: 620px) {
    .q-btn { min-height: 44px; }
    .q-form { flex-direction: column; align-items: stretch; }
    .q-form input { min-width: 0; width: 100%; min-height: 44px; }
  }
</style>