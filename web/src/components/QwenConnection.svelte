<script lang="ts">
  import { onDestroy } from 'svelte'

  type Flow = { id: string; url: string; user_code: string }
  type Status = { connected: boolean; loaded: boolean; models: number; flow?: Flow; message?: string }

  let panelOpen = false
  let token = ''
  let status: Status | null = null
  let busy = false
  let error = ''
  let poll: ReturnType<typeof setInterval> | undefined

  async function request<T>(path: string, method = 'GET'): Promise<T> {
    const res = await fetch('/api/qwen/' + path, {
      method, headers: { Authorization: 'Bearer ' + token.trim() }, cache: 'no-store',
    })
    if (res.status === 401) status = null
    if (res.status === 204) return undefined as T
    const result = await res.json()
    if (!res.ok) throw new Error(result.error || 'Could not reach the Qwen sign-in controls.')
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
      if (!poll) {
        poll = setInterval(() => {
          if (!panelOpen || !status) return
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

  onDestroy(() => { if (poll) clearInterval(poll) })
</script>

<section class="qwen" aria-label="Qwen subscription">
  <button type="button" class="toggle" aria-expanded={panelOpen} on:click={() => { panelOpen = !panelOpen }}>
    {status?.connected ? 'Qwen connected' : 'Connect Qwen'}
  </button>
  {#if panelOpen}
    <div class="contents">
      {#if !status}
        <form on:submit|preventDefault={unlock}>
          <label>Dashboard operator token
            <input type="password" autocomplete="off" bind:value={token} required />
          </label>
          <button type="submit" disabled={busy}>Continue</button>
        </form>
      {:else}
        {#if status.flow}
          <p role="status">Waiting for approval in Qwen.</p>
          <p>User code <code>{status.flow.user_code}</code></p>
          <a href={status.flow.url} target="_blank" rel="noopener noreferrer">Approve on Qwen ↗</a>
          <button type="button" disabled={busy} on:click={() => status?.flow && cancel(status.flow)}>Cancel</button>
        {:else}
          {#if status.connected}
            {#if !status.loaded}
              <p role="status">Signed in. Restart NEXUS to load the Qwen provider.</p>
            {:else if status.models}
              <p role="status">Connected. {status.models} Qwen {status.models === 1 ? 'model' : 'models'} ready.</p>
            {:else}
              <p role="status">Signed in. Qwen model discovery is pending or unavailable.</p>
            {/if}
          {:else}
            <p role="status">Sign in with a Qwen account that has a Code subscription.</p>
          {/if}
          <button type="button" disabled={busy} on:click={start}>{status.connected ? 'Reconnect Qwen' : 'Start Qwen sign-in'}</button>
        {/if}
        {#if status.message}<p role="status">{status.message}</p>{/if}
      {/if}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
    </div>
  {/if}
</section>

<style>
  .qwen { margin-bottom: 1rem; }
  .contents { padding: .75rem 1rem; margin-top: .5rem; border: 1px solid #374151; border-radius: .5rem; }
  form { display: flex; flex-wrap: wrap; gap: .75rem; align-items: end; }
  label { display: grid; gap: .25rem; }
  input, button, a { font: inherit; }
  input, button { color: inherit; background: #151724; border: 1px solid #45485d; border-radius: .35rem; padding: .35rem .6rem; }
  button { cursor: pointer; }
  button:disabled { cursor: wait; opacity: .65; }
  .toggle, a { color: #c4b5fd; }
  p { font-size: .875rem; margin: 0 0 .5rem; }
  .error { color: #fca5a5; margin-top: .5rem; }
  a { margin-right: .75rem; }
</style>
