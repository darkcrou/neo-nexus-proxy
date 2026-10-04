<script lang="ts">
  import { onMount, onDestroy } from 'svelte'
  import { Chart } from 'chart.js/auto'
  import {
    connectSSE, disconnectSSE, fetchInitial, fetchTimeseries, fetchSavings,
    connected, stats, recentRequests, providers, providerBreakdown, complexityMix, timeseries, savings,
  } from './stores/requests'
  import {
    fetchUsage, refreshUsage, fetchUsageEvents, usageWindows, usageEvents,
    type UsageWindow,
  } from './stores/usage'

  const CX_COLOR: Record<string, string> = {
    simple: '#10b981', standard: '#06b6d4', complex: '#7c3aed', critical: '#ef4444',
  }

  const REPO = 'https://github.com/lynuxis2026-pixel/nexus-proxy'
  $: shareText = `I've saved $${$savings.saved_usd.toFixed(2)} on Claude Code with NEXUS 🚀 (${Math.round($savings.percent_saved)}% cheaper than Claude) — open-source smart LLM proxy`
  $: shareUrl = `https://twitter.com/intent/tweet?text=${encodeURIComponent(shareText)}&url=${encodeURIComponent(REPO)}`

  const PALETTE = ['#7c3aed', '#06b6d4', '#10b981', '#f59e0b', '#ef4444', '#3b82f6', '#ec4899', '#84cc16', '#a855f7', '#14b8a6']

  // ── First-run setup wizard ──
  type Recommended = { name: string; tier: string; note: string }
  let setupChecked = false
  let setupOpen = false
  let setupStep = 0
  let setupStatus: any = null
  let setupEntries: { name: string; api_key: string; configured: boolean; tested: 'idle' | 'ok' | 'err' | 'busy'; err?: string }[] = []
  let setupSaving = false
  let setupDone = false

  async function loadSetup() {
    try {
      const r = await fetch('/api/setup/status')
      setupStatus = await r.json()
    } catch { setupStatus = null }
    if (setupStatus?.first_run) {
      // Seed the entries: env-discovered first (key already in env, no paste needed),
      // then the recommended starter set.
      const seen = new Set<string>()
      setupEntries = []
      for (const n of (setupStatus.discoverable || [])) {
        setupEntries.push({ name: n, api_key: '', configured: true, tested: 'idle' })
        seen.add(n)
      }
      for (const r of (setupStatus.recommended || [])) {
        if (!seen.has(r.name)) {
          setupEntries.push({ name: r.name, api_key: '', configured: false, tested: 'idle' })
          seen.add(r.name)
        }
      }
      setupOpen = true
    }
    setupChecked = true
  }
  function setupNext() { if (setupStep < 3) setupStep++ }
  function setupBack() { if (setupStep > 0) setupStep-- }
  async function setupTestOne(i: number) {
    const e = setupEntries[i]
    if (!e.configured && !e.api_key.trim()) { e.tested = 'idle'; setupEntries = setupEntries; return }
    e.tested = 'busy'; setupEntries = setupEntries
    try {
      const r = await fetch('/api/setup/test', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: e.name, api_key: e.api_key.trim() }),
      })
      const j = await r.json()
      e.tested = j.ok ? 'ok' : 'err'; e.err = j.error
    } catch (err) { e.tested = 'err'; e.err = String(err) }
    setupEntries = setupEntries
  }
  async function setupTestAll() {
    for (let i = 0; i < setupEntries.length; i++) await setupTestOne(i)
  }
  async function setupFinish(skip = false) {
    setupSaving = true
    const payload = skip
      ? { skip: true }
      : { providers: setupEntries.filter(e => e.api_key.trim() || e.configured).map(e => ({ name: e.name, api_key: e.api_key.trim() })) }
    try {
      await fetch('/api/setup/save', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      })
      setupDone = true
      setTimeout(() => { setupOpen = false; setupDone = false }, 1200)
    } catch (e) { console.error('save failed', e) }
    setupSaving = false
  }
  $: connectSnippet = setupStatus?.platform === 'windows'
    ? `$env:ANTHROPIC_BASE_URL = "http://localhost:${setupStatus?.proxy_port || 3000}"\n$env:ANTHROPIC_API_KEY  = "nexus-local"\nclaude`
    : `export ANTHROPIC_BASE_URL=http://localhost:${setupStatus?.proxy_port || 3000}\nexport ANTHROPIC_API_KEY=nexus-local\nclaude`
  $: tierColor = (t: string) => ({ free: '#10b981', standard: '#06b6d4', premium: '#7c3aed' } as any)[t] || '#64748b'

  function copyText(t: string) { navigator.clipboard?.writeText(t) }

  // ── Playground ──
  type PgModel = { provider: string; tier: string; model: string; label: string }
  type Turn = { role: 'user' | 'assistant'; content: string }
  let pgOpen = false
  let pgModels: PgModel[] = []
  let pgSelected = ''
  let pgTurns: Turn[] = []
  let pgInput = ''
  let pgStreaming = false
  let pgErr = ''

  async function pgLoadModels() {
    try {
      const r = await fetch('/api/playground/models')
      const j = await r.json()
      pgModels = j.models || []
      if (pgModels.length && !pgSelected) pgSelected = pgModels[0].model
    } catch (e) { pgErr = String(e) }
  }
  function pgOpenChat() { pgOpen = true; pgLoadModels() }
  function pgClose() { pgOpen = false }

  async function pgSend() {
    if (!pgInput.trim() || pgStreaming || !pgSelected) return
    const userTurn: Turn = { role: 'user', content: pgInput.trim() }
    pgTurns = [...pgTurns, userTurn, { role: 'assistant', content: '' }]
    pgInput = ''; pgStreaming = true; pgErr = ''
    const sel = pgModels.find(m => m.model === pgSelected)
    const payload = {
      model: pgSelected,
      max_tokens: 4096,
      messages: pgTurns.filter(t => t.content || t.role === 'user').slice(0, -1).map(t => ({ role: t.role, content: t.content })),
    }
    try {
      const resp = await fetch('/api/playground/chat', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(sel ? { 'X-Nexus-Provider': sel.provider } : {}),
        },
        body: JSON.stringify(payload),
      })
      if (!resp.ok || !resp.body) { pgErr = `HTTP ${resp.status}`; pgStreaming = false; return }
      const reader = resp.body.getReader()
      const dec = new TextDecoder()
      let buf = ''
      while (true) {
        const { value, done } = await reader.read()
        if (done) break
        buf += dec.decode(value, { stream: true })
        let nl: number
        while ((nl = buf.indexOf('\n')) >= 0) {
          const line = buf.slice(0, nl); buf = buf.slice(nl + 1)
          if (!line.startsWith('data:')) continue
          const data = line.slice(5).trim()
          if (!data || data === '[DONE]') continue
          try {
            const evt = JSON.parse(data)
            const delta = evt?.delta?.text ?? evt?.delta?.content ?? evt?.content_block?.text ?? ''
            if (delta) {
              pgTurns[pgTurns.length - 1].content += delta
              pgTurns = pgTurns
            }
          } catch { /* tolerate non-JSON keep-alive events */ }
        }
      }
    } catch (e) { pgErr = String(e) }
    pgStreaming = false
  }
  function pgKey(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); pgSend() }
  }
  function pgReset() { pgTurns = []; pgErr = '' }

  // ── Request inspector + replay ──
  let inspecting: any = null
  let inspectLoading = false
  let replayProvider = ''
  let replayResult: any = null
  let replayLoading = false

  function pretty(s: string): string {
    try { return JSON.stringify(JSON.parse(s), null, 2) } catch { return s }
  }
  async function openInspector(id: number) {
    inspecting = null; replayResult = null; inspectLoading = true
    try {
      const r = await fetch(`/api/requests/${id}`)
      inspecting = await r.json()
    } catch { inspecting = { error: 'failed to load' } }
    inspectLoading = false
  }
  function closeInspector() { inspecting = null; replayResult = null }
  async function runReplay() {
    if (!inspecting?.id) return
    replayLoading = true; replayResult = null
    try {
      const r = await fetch('/api/replay', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id: inspecting.id, provider: replayProvider }),
      })
      replayResult = await r.json()
    } catch (e) { replayResult = { error: String(e) } }
    replayLoading = false
  }

  // ── Team leaderboard ──
  let leaderboard: any[] = []
  async function loadLeaderboard() {
    try {
      const r = await fetch('/api/leaderboard')
      const j = await r.json()
      leaderboard = j.leaderboard || []
    } catch { /* ignore */ }
  }
  $: namedBoard = leaderboard.filter((e) => e.user && e.user !== 'unattributed')

  // ── Usage windows panel ──
  let drillProvider: string | null = null
  let drillLoading = false
  let drillErr = ''
  let usageRefreshing = false

  // Most recent window per provider, kept only while it is still open.
  function openCurrentWindows(ws: UsageWindow[]): UsageWindow[] {
    const seen = new Set<string>()
    const cur: UsageWindow[] = []
    for (const w of ws) {
      if (seen.has(w.provider)) continue
      seen.add(w.provider)
      if (!w.ended_at) cur.push(w)
    }
    return cur
  }
  $: currentWindows = openCurrentWindows($usageWindows)

  // Newest ~8 windows across all providers, for the compact history table.
  $: recentWindows = $usageWindows
    .slice()
    .sort((a, b) => Date.parse(b.started_at) - Date.parse(a.started_at))
    .slice(0, 8)

  const END_REASON_STYLE: Record<string, string> = {
    '': 'open', RATE_LIMIT: 'limit', PROVIDER_RESET: 'reset', TIME_ELAPSED: 'elapsed', UNKNOWN: 'unknown',
  }
  const END_REASON_LABEL: Record<string, string> = {
    '': 'open', RATE_LIMIT: 'rate limit', PROVIDER_RESET: 'reset', TIME_ELAPSED: 'elapsed', UNKNOWN: 'unknown',
  }
  const ercStyle = (w: UsageWindow) => END_REASON_STYLE[w.end_reason] || 'unknown'
  const ercLabel = (w: UsageWindow) => END_REASON_LABEL[w.end_reason] || w.end_reason.toLowerCase()

  // Compact token rendering for window/event numbers.
  function fmtTokens(n: number): string {
    if (n >= 1_000_000) return (n / 1_000_000).toFixed(1).replace(/\.0$/, '') + 'M'
    if (n >= 1_000) return (n / 1_000).toFixed(1).replace(/\.0$/, '') + 'k'
    return String(n)
  }
  // null tokens mean the provider didn't report them — show "–", never 0.
  const tok = (v: number | null) => (v === null ? '–' : fmtTokens(v))

  // "14:32" today, "May 12 14:32" otherwise.
  function fmtWinTime(iso: string): string {
    const d = new Date(iso)
    if (isNaN(d.getTime())) return iso
    const hm = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    if (d.toDateString() === new Date().toDateString()) return hm
    return `${d.toLocaleDateString([], { month: 'short', day: 'numeric' })} ${hm}`
  }

  // Utilization bar color: closer to the reported limit → hotter.
  const quotaColor = (u: number) => (u >= 0.9 ? '#ef4444' : u >= 0.7 ? '#f59e0b' : '#06b6d4')

  async function toggleDrill(provider: string) {
    if (drillProvider === provider) { drillProvider = null; return }
    drillProvider = provider
    drillLoading = true
    drillErr = ''
    const ok = await fetchUsageEvents(provider)
    drillLoading = false
    if (!ok) drillErr = 'failed to load events — try the refresh button'
  }

  async function refreshUsagePanel() {
    usageRefreshing = true
    await Promise.all([
      refreshUsage(),
      drillProvider ? fetchUsageEvents(drillProvider) : Promise.resolve(true),
    ])
    usageRefreshing = false
  }

  let costCanvas: HTMLCanvasElement
  let provCanvas: HTMLCanvasElement
  let costChart: Chart | undefined
  let provChart: Chart | undefined
  let poll: ReturnType<typeof setInterval>

  const gridColor = '#1a2035'
  const tickColor = '#64748b'

  onMount(() => {
    loadSetup()
    fetchInitial()
    fetchUsage()
    connectSSE()

    costChart = new Chart(costCanvas, {
      type: 'line',
      data: { labels: [], datasets: [{
        label: 'Cost (USD)', data: [], borderColor: '#7c3aed',
        backgroundColor: 'rgba(124,58,237,0.15)', fill: true, tension: 0.35, pointRadius: 2,
      }] },
      options: {
        responsive: true, maintainAspectRatio: false,
        plugins: { legend: { labels: { color: tickColor, font: { size: 10 } } } },
        scales: {
          x: { ticks: { color: tickColor, maxTicksLimit: 8, font: { size: 9 } }, grid: { color: gridColor } },
          y: { ticks: { color: tickColor, font: { size: 9 } }, grid: { color: gridColor }, beginAtZero: true },
        },
      },
    })

    provChart = new Chart(provCanvas, {
      type: 'doughnut',
      data: { labels: [], datasets: [{ data: [], backgroundColor: PALETTE, borderColor: '#0a0e1a', borderWidth: 2 }] },
      options: {
        responsive: true, maintainAspectRatio: false, cutout: '62%',
        plugins: { legend: { position: 'right', labels: { color: tickColor, font: { size: 10 }, boxWidth: 12 } } },
      },
    })

    loadLeaderboard()
    poll = setInterval(() => { fetchTimeseries(); fetchSavings(); loadLeaderboard() }, 10000)
  })

  onDestroy(() => {
    disconnectSSE()
    clearInterval(poll)
    costChart?.destroy()
    provChart?.destroy()
  })

  // Live updates.
  $: if (costChart && $timeseries) {
    costChart.data.labels = $timeseries.map(b => b.bucket)
    costChart.data.datasets[0].data = $timeseries.map(b => b.cost_usd)
    costChart.update('none')
  }
  $: if (provChart && $providerBreakdown) {
    const labels = Object.keys($providerBreakdown)
    provChart.data.labels = labels
    provChart.data.datasets[0].data = labels.map(k => $providerBreakdown[k].count)
    provChart.update('none')
  }
</script>

{#if setupOpen}
  <div class="setup-bg">
    <div class="setup-card">
      <div class="setup-head">
        <div class="setup-logo">NE<span>X</span>US</div>
        <div class="setup-steps">
          {#each ['Welcome','Providers','Test','Connect'] as label, i (i)}
            <span class="dot" class:on={i === setupStep} class:done={i < setupStep}></span>
          {/each}
        </div>
        <button class="skip" on:click={() => setupFinish(true)} disabled={setupSaving}>Skip</button>
      </div>

      {#if setupDone}
        <div class="setup-done">✓ Saved. Welcome to NEXUS.</div>
      {:else if setupStep === 0}
        <h2>Let's get you set up in 2 minutes</h2>
        <p class="lead">
          NEXUS routes Claude Code, Cursor and aider to the cheapest capable model — without your code
          leaving your machine. Typical user saves <b style="color:#10b981">~$200+/month</b>.
        </p>
        <ul class="bullets">
          <li>🔒 Privacy firewall masks secrets before they leave</li>
          <li>🪜 Cheap-first cascade verifies cheap answers and only escalates on failure</li>
          <li>🧠 Adaptive routing learns which provider wins YOUR tasks</li>
        </ul>
        <div class="setup-actions">
          <span></span>
          <button class="setup-cta" on:click={setupNext}>Let's go →</button>
        </div>
      {:else if setupStep === 1}
        <h2>Pick at least one provider</h2>
        <p class="lead">
          {#if setupStatus?.discoverable?.length}
            We auto-detected <b>{setupStatus.discoverable.length}</b> from your environment variables ✓.
          {:else}
            We recommend starting with <b style="color:#10b981">Groq</b> (free) and <b style="color:#06b6d4">DeepSeek</b> ($0.27/M).
          {/if}
        </p>
        <div class="prov-list">
          {#each setupEntries as e (e.name)}
            <div class="prov-row">
              <div class="prov-name">{e.name}</div>
              {#if e.configured}
                <div class="prov-env">✓ key from environment</div>
              {:else}
                <input type="password" placeholder="paste API key (or leave empty to skip)" bind:value={e.api_key} class="prov-input" />
              {/if}
            </div>
          {/each}
        </div>
        <div class="setup-actions">
          <button class="setup-ghost" on:click={setupBack}>← Back</button>
          <button class="setup-cta" on:click={setupNext}>Next → Test</button>
        </div>
      {:else if setupStep === 2}
        <h2>Test your providers</h2>
        <p class="lead">We'll ping each provider's <code class="mono">/models</code> endpoint to verify the key works.</p>
        <div class="prov-list">
          {#each setupEntries as e, i (e.name)}
            {#if e.configured || e.api_key.trim()}
              <div class="prov-row test">
                <div class="prov-name">{e.name}</div>
                <div class="test-status">
                  {#if e.tested === 'busy'}<span class="muted">testing…</span>
                  {:else if e.tested === 'ok'}<span class="ok">✓ ok</span>
                  {:else if e.tested === 'err'}<span class="err" title={e.err}>✗ {e.err?.slice(0,40)}…</span>
                  {:else}<span class="muted">idle</span>{/if}
                </div>
                <button class="setup-ghost small" on:click={() => setupTestOne(i)}>Test</button>
              </div>
            {/if}
          {/each}
        </div>
        <div class="setup-actions">
          <button class="setup-ghost" on:click={setupBack}>← Back</button>
          <div style="display:flex; gap:8px">
            <button class="setup-ghost" on:click={setupTestAll}>Test all</button>
            <button class="setup-cta" on:click={setupNext}>Next →</button>
          </div>
        </div>
      {:else if setupStep === 3}
        <h2>Connect your coding tool</h2>
        <p class="lead">Two env vars and you're done. NEXUS speaks both the Anthropic + OpenAI APIs.</p>
        <div class="codeblock">
          <pre>{connectSnippet}</pre>
          <button class="copy-btn" on:click={() => copyText(connectSnippet)}>copy</button>
        </div>
        <p class="hint">
          Already running <code class="mono">nexus start</code>? Restart it once so the proxy picks up your providers.
          Then run <code class="mono">claude</code> in any project — it'll route through NEXUS automatically.
        </p>
        <div class="setup-actions">
          <button class="setup-ghost" on:click={setupBack}>← Back</button>
          <button class="setup-cta" on:click={() => setupFinish(false)} disabled={setupSaving}>
            {setupSaving ? 'Saving…' : 'Finish & open dashboard →'}
          </button>
        </div>
      {/if}
    </div>
  </div>
{/if}

<header>
  <h1>NE<span>X</span>US</h1>
  <div class="header-right">
    <button class="pg-open" on:click={pgOpenChat} title="Pick any model and chat with it">💬 Playground</button>
    <div class="status" class:live={$connected}>
      <span class="dot"></span>{$connected ? 'live' : 'connecting'}
    </div>
  </div>
</header>

{#if pgOpen}
  <div class="pg-bg" role="presentation" on:click={pgClose}>
    <div class="pg-card" role="dialog" aria-modal="true" on:click|stopPropagation on:keydown|stopPropagation>
      <div class="pg-head">
        <div class="pg-title">
          <b>💬 Playground</b>
          <span class="muted">— pick a model, chat with it through NEXUS</span>
        </div>
        <div class="pg-controls">
          <select class="pg-select" bind:value={pgSelected} disabled={pgStreaming}>
            {#each pgModels as m (m.provider + '/' + m.model)}
              <option value={m.model}>{m.label}</option>
            {/each}
            {#if pgModels.length === 0}
              <option value="">no providers configured — run the setup wizard</option>
            {/if}
          </select>
          <button class="pg-ghost small" on:click={pgReset} disabled={pgStreaming || pgTurns.length === 0}>Clear</button>
          <button class="x" on:click={pgClose}>✕</button>
        </div>
      </div>

      <div class="pg-feed">
        {#if pgTurns.length === 0}
          <div class="pg-empty">
            <div class="pg-empty-title">No messages yet</div>
            <div class="pg-empty-sub">Pick a model above, type something below. Streamed through NEXUS — every request is logged + costed in your dashboard.</div>
          </div>
        {/if}
        {#each pgTurns as t, i (i)}
          <div class="pg-turn" class:user={t.role === 'user'} class:assistant={t.role === 'assistant'}>
            <div class="pg-who">{t.role === 'user' ? 'you' : (pgModels.find(m => m.model === pgSelected)?.provider || 'assistant')}</div>
            <div class="pg-body">{t.content || (pgStreaming && i === pgTurns.length - 1 ? '…' : '')}</div>
          </div>
        {/each}
        {#if pgErr}
          <div class="pg-err">⚠ {pgErr}</div>
        {/if}
      </div>

      <div class="pg-input">
        <textarea bind:value={pgInput} on:keydown={pgKey} rows="2" placeholder="Type a message — Enter to send, Shift+Enter for a new line" disabled={pgStreaming || !pgSelected}></textarea>
        <button class="pg-send" on:click={pgSend} disabled={pgStreaming || !pgInput.trim() || !pgSelected}>
          {pgStreaming ? 'Streaming…' : 'Send →'}
        </button>
      </div>
    </div>
  </div>
{/if}

<main>
  {#if $savings.saved_usd > 0}
    <div class="savings">
      <div class="savings-msg">
        💸 You've saved <b>${$savings.saved_usd.toFixed(2)}</b> this month —
        <b>{Math.round($savings.percent_saved)}%</b> cheaper than Claude
        {#if $savings.cache_saved_usd > 0}<span class="cache-note">(incl. <b>${$savings.cache_saved_usd.toFixed(2)}</b> from caching)</span>{/if}
      </div>
      <div class="savings-actions">
        <a class="btn" href={shareUrl} target="_blank" rel="noopener">Share on X</a>
        <a class="btn ghost" href="/api/savings/card.svg" target="_blank" rel="noopener">Download card</a>
      </div>
    </div>
  {/if}

  <div class="stats">
    <div class="card"><span class="num accent">{$stats.total_requests}</span><span class="label">requests today</span></div>
    <div class="card"><span class="num green">${$stats.total_cost_usd.toFixed(4)}</span><span class="label">cost today</span></div>
    <div class="card"><span class="num cyan">${($stats.cache_saved_usd || 0).toFixed(4)}</span><span class="label">cache saved today</span></div>
    <div class="card"><span class="num">${$stats.forecast_usd.toFixed(2)}</span><span class="label">forecast / month</span></div>
    <div class="card"><span class="num">{Math.round($stats.avg_latency_ms)}ms</span><span class="label">avg latency</span></div>
    <div class="card" title="Secrets & PII masked by the privacy firewall before requests left your machine">
      <span class="num amber">🔒 {$stats.redacted_total || 0}</span><span class="label">secrets masked · 0 leaked</span>
    </div>
  </div>

  {#if $providers.length}
    <div class="providers">
      {#each $providers as p (p.name)}
        <div class="chip" class:up={p.healthy} class:down={!p.healthy}>
          <span class="dot"></span>{p.name}<span class="tier">{p.tier}</span>
        </div>
      {/each}
    </div>
  {/if}

  {#if $recentRequests.length}
    <div class="panel mix-panel">
      <div class="panel-title">Routing mix · last {$recentRequests.length} requests</div>
      <div class="mixbar">
        {#each $complexityMix as seg (seg.key)}
          {#if seg.pct > 0}
            <div class="seg" style="width:{seg.pct}%; background:{CX_COLOR[seg.key]}"
                 title="{seg.key}: {seg.count} ({Math.round(seg.pct)}%)"></div>
          {/if}
        {/each}
      </div>
      <div class="mixlegend">
        {#each $complexityMix as seg (seg.key)}
          <span class="mleg"><span class="mdot" style="background:{CX_COLOR[seg.key]}"></span>{seg.key} <b>{seg.count}</b></span>
        {/each}
      </div>
    </div>
  {/if}

  <div class="charts">
    <div class="panel">
      <div class="panel-title">Cost — last 24h</div>
      <div class="canvas-wrap"><canvas bind:this={costCanvas}></canvas></div>
    </div>
    <div class="panel">
      <div class="panel-title">Requests by provider</div>
      <div class="canvas-wrap"><canvas bind:this={provCanvas}></canvas></div>
    </div>
  </div>

  <!-- ══ Usage windows: provider quota burn-down + event drill-down ══ -->
  {#if $usageWindows.length}
    <div class="panel usage-panel">
      <div class="usage-head">
        <div class="panel-title">Usage windows</div>
        <button class="usage-refresh" on:click={refreshUsagePanel} disabled={usageRefreshing}
                title="Refresh usage data" aria-label="Refresh usage data">
          {usageRefreshing ? 'refreshing…' : '↻ refresh'}
        </button>
      </div>

      <!-- Current (open) window per provider: live token burn + provider-reported quota -->
      {#if currentWindows.length}
        <div class="uw-cards">
          {#each currentWindows as w (w.provider)}
            <div class="uw-card">
              <div class="uw-card-head">
                <span class="uw-prov">{w.provider}</span>
                <span class="muted">since {fmtWinTime(w.started_at)}</span>
              </div>
              <div class="uw-stats">
                <span><b>{fmtTokens(w.in_tokens)}</b> in</span>
                <span><b>{fmtTokens(w.cache_read_tokens)}</b> cache read</span>
                <span><b>{fmtTokens(w.out_tokens)}</b> out</span>
              </div>
              <div class="uw-meta">
                <span>cache hit <b>{Math.round(w.cache_hit_ratio * 100)}%</b></span>
                <span>{w.requests} req</span>
              </div>
              {#if w.quota}
                <!-- utilization is transcribed from the provider, never computed locally -->
                <div class="uw-quota">
                  <div class="uw-quota-label">
                    <span class="muted">provider-reported · {w.quota.dimension}</span>
                    <b>{Math.round(w.quota.utilization * 100)}%</b>
                  </div>
                  <div class="uw-bar">
                    <div class="uw-fill" style="width:{w.quota.utilization * 100}%; background:{quotaColor(w.quota.utilization)}"></div>
                  </div>
                  {#if w.quota.reset_at}
                    <div class="uw-reset">resets {fmtWinTime(w.quota.reset_at)}</div>
                  {/if}
                </div>
              {:else}
                <div class="uw-noquota">no provider quota</div>
              {/if}
            </div>
          {/each}
        </div>
      {/if}

      <!-- Recent windows table — click a row to drill into that provider's events -->
      <div class="uw-rows">
        {#each recentWindows as w (w.provider + '|' + w.started_at)}
          <div class="uw-row clickable" role="button" tabindex="0"
               class:active={drillProvider === w.provider}
               title="Show recent usage events for {w.provider}"
               on:click={() => toggleDrill(w.provider)}
               on:keydown={(e) => e.key === 'Enter' && toggleDrill(w.provider)}>
            <span class="uw-r-prov">{w.provider}</span>
            <span class="uw-r-time">{fmtWinTime(w.started_at)} → {w.ended_at ? fmtWinTime(w.ended_at) : 'open'}</span>
            <span class="erc {ercStyle(w)}">{ercLabel(w)}</span>
            <span class="uw-r-req">{w.requests}</span>
            <span class="uw-r-tok">{fmtTokens(w.total_tokens)}</span>
          </div>
        {/each}
      </div>

      <!-- Drill-down: recent usage events for the selected provider -->
      {#if drillProvider}
        <div class="uw-drill">
          <div class="uw-drill-head">
            <span>events · {drillProvider} · last 50</span>
            <button class="x" on:click={() => (drillProvider = null)} aria-label="Close drill-down">✕</button>
          </div>
          {#if drillLoading}
            <div class="uw-drill-note">loading events…</div>
          {:else if drillErr}
            <div class="uw-drill-note err">{drillErr}</div>
          {:else if $usageEvents.length === 0}
            <div class="uw-drill-note">no events recorded for this provider</div>
          {:else}
            <div class="uw-ev uw-ev-head">
              <span>time</span><span>model</span><span>status</span>
              <span>in</span><span>out</span><span class="uw-ev-cache">cache</span><span></span>
            </div>
            {#each $usageEvents as ev (ev.id)}
              <div class="uw-ev">
                <span class="uw-ev-time" title={ev.created_at}>{fmtWinTime(ev.created_at)}</span>
                <span class="uw-ev-model" title={ev.model_used}>{ev.model_used}</span>
                <span class="uw-ev-status" class:ok={ev.success} class:bad={!ev.success}>{ev.status === 0 ? 'err' : ev.status}</span>
                <span class="uw-ev-tok" class:nil={ev.in_tokens === null}>{tok(ev.in_tokens)}</span>
                <span class="uw-ev-tok" class:nil={ev.out_tokens === null}>{tok(ev.out_tokens)}</span>
                <span class="uw-ev-tok uw-ev-cache" class:nil={ev.cache_read_tokens === null}>{tok(ev.cache_read_tokens)}</span>
                <span class="uw-ev-badges">
                  {#if ev.probe}<span class="evb probe">probe</span>{/if}
                  {#if ev.rate_limited}<span class="evb limit">429</span>{/if}
                  {#if ev.usage_partial}<span class="evb partial">partial</span>{/if}
                </span>
              </div>
            {/each}
          {/if}
        </div>
      {/if}
    </div>
  {/if}

  {#if namedBoard.length}
    <div class="feed-title">Team leaderboard · saved this month</div>
    <div class="board">
      {#each namedBoard as e, i (e.user)}
        <div class="brow">
          <span class="rank">#{i + 1}</span>
          <span class="buser">{e.user}</span>
          <span class="breq">{e.requests} req</span>
          <span class="bsaved">${e.saved_usd.toFixed(2)}</span>
        </div>
      {/each}
    </div>
  {/if}

  <div class="feed-title">Live request feed</div>
  <div class="feed">
    {#if $recentRequests.length === 0}
      <div class="empty">Waiting for requests… point Claude Code at this proxy.</div>
    {/if}
    {#each $recentRequests as req (req.id)}
      <div class="row clickable" role="button" tabindex="0" title="Inspect / replay"
           on:click={() => openInspector(req.id)} on:keydown={(e) => e.key === 'Enter' && openInspector(req.id)}>
        <span class="provider">{req.provider}</span>
        <span class="model">{req.model_asked}</span>
        <span class="cx {req.complexity}">{req.complexity}</span>
        <span class="tokens">{req.input_tokens + req.output_tokens}t</span>
        <span class="cost">${req.cost_usd.toFixed(5)}</span>
        <span class="latency">{req.latency_ms}ms</span>
        <span class="code" class:err={req.status >= 400}>{req.status}</span>
      </div>
    {/each}
  </div>
</main>

{#if inspecting}
  <div class="modal-bg" role="presentation" on:click={closeInspector}>
    <div class="modal" role="dialog" aria-modal="true" on:click|stopPropagation on:keydown|stopPropagation>
      <div class="modal-head">
        <div>
          <b>Request #{inspecting.id}</b>
          <span class="muted">· {inspecting.provider} · {inspecting.model_used} · ${(inspecting.cost_usd || 0).toFixed(5)} · {inspecting.latency_ms}ms</span>
        </div>
        <button class="x" on:click={closeInspector}>✕</button>
      </div>

      {#if !inspecting.inspected}
        <div class="hint">No prompt/response captured. Start NEXUS with <code>--inspect</code> to enable the inspector + replay.</div>
      {:else}
        <div class="io-grid">
          <div>
            <div class="io-label">PROMPT</div>
            <pre>{pretty(inspecting.prompt)}</pre>
          </div>
          <div>
            <div class="io-label">RESPONSE</div>
            <pre>{pretty(inspecting.response)}</pre>
          </div>
        </div>

        <div class="replay">
          <span class="io-label">REPLAY vs</span>
          <select bind:value={replayProvider}>
            <option value="">auto-route</option>
            {#each $providers as p (p.name)}<option value={p.name}>{p.name}</option>{/each}
          </select>
          <button class="btn-replay" on:click={runReplay} disabled={replayLoading}>
            {replayLoading ? 'Replaying…' : 'Replay'}
          </button>
          {#if replayResult}
            {#if replayResult.error}
              <span class="err">{replayResult.error}</span>
            {:else}
              <span class="muted">→ {replayResult.provider} · {replayResult.model} · {(replayResult.input_tokens || 0) + (replayResult.output_tokens || 0)}t · {replayResult.latency_ms}ms</span>
            {/if}
          {/if}
        </div>
        {#if replayResult && !replayResult.error}
          <div class="io-label">REPLAY OUTPUT</div>
          <pre class="replay-out">{replayResult.output}</pre>
        {/if}
      {/if}
    </div>
  </div>
{/if}

<style>
  :global(*, *::before, *::after) { box-sizing: border-box; margin: 0; padding: 0; }
  :global(body) {
    background: #050816; color: #e2e8f0;
    font-family: 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
    font-size: 13px;
  }
  header { display: flex; align-items: center; justify-content: space-between; max-width: 1100px; margin: 0 auto; padding: 28px 28px 0; }
  h1 { font-size: 34px; font-weight: 800; letter-spacing: -0.04em; color: #7c3aed; }
  h1 span { color: #06b6d4; }
  .status { font-size: 11px; letter-spacing: 0.15em; color: #ef4444; display: flex; align-items: center; gap: 8px; text-transform: uppercase; }
  .status.live { color: #10b981; }
  .dot { width: 8px; height: 8px; border-radius: 50%; background: currentColor; box-shadow: 0 0 8px currentColor; }
  .status.live .dot { animation: pulse 1.6s infinite; }
  @keyframes pulse { 0%,100% { opacity: 1; } 50% { opacity: 0.35; } }

  main { max-width: 1100px; margin: 0 auto; padding: 20px 28px 40px; }

  .savings {
    display: flex; align-items: center; justify-content: space-between; gap: 16px; flex-wrap: wrap;
    background: linear-gradient(90deg, rgba(124,58,237,0.16), rgba(6,182,212,0.12));
    border: 1px solid #2a2150; border-radius: 10px; padding: 14px 18px; margin-bottom: 16px;
  }
  .savings-msg { font-size: 15px; color: #e2e8f0; }
  .savings-msg b { color: #10b981; }
  .savings-actions { display: flex; gap: 8px; }
  .btn {
    display: inline-block; padding: 7px 14px; border-radius: 6px; font-size: 13px; font-weight: 600;
    text-decoration: none; background: #7c3aed; color: #fff; border: 1px solid #7c3aed;
  }
  .btn:hover { background: #6d28d9; }
  .btn.ghost { background: transparent; color: #94a3b8; border-color: #1a2035; }
  .btn.ghost:hover { color: #e2e8f0; border-color: #2a2150; }

  .stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 14px; margin-bottom: 14px; }
  .card { display: flex; flex-direction: column; gap: 4px; background: #0a0e1a; border: 1px solid #1a2035; border-radius: 8px; padding: 18px 20px; }
  .num { font-size: 26px; font-weight: 700; font-family: 'Geist Mono', monospace; }
  .num.accent { color: #7c3aed; } .num.green { color: #10b981; } .num.cyan { color: #06b6d4; } .num.amber { color: #f59e0b; }
  .cache-note { color: #06b6d4; font-size: 13px; }

  .clickable { cursor: pointer; }
  .clickable:hover { background: #11182f; }
  .modal-bg { position: fixed; inset: 0; background: rgba(0,0,0,0.6); display: flex; align-items: center; justify-content: center; padding: 24px; z-index: 50; }
  .modal { background: #0a0e1a; border: 1px solid #1a2035; border-radius: 12px; width: 100%; max-width: 920px; max-height: 86vh; overflow: auto; padding: 18px 20px; }
  .modal-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 14px; }
  .modal-head .muted { color: #64748b; font-size: 12px; }
  .x { background: #1a2035; border: 0; color: #e2e8f0; border-radius: 6px; width: 28px; height: 28px; cursor: pointer; }
  .io-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
  .io-label { font-size: 10px; color: #64748b; letter-spacing: 0.14em; margin: 8px 0 4px; }
  .modal pre { background: #050816; border: 1px solid #1a2035; border-radius: 8px; padding: 10px; font-family: 'Geist Mono','SF Mono',ui-monospace,monospace; font-size: 11px; color: #94a3b8; white-space: pre-wrap; word-break: break-word; max-height: 320px; overflow: auto; }
  .replay { display: flex; align-items: center; gap: 8px; margin-top: 14px; flex-wrap: wrap; }
  .replay select { background: #0a0e1a; color: #e2e8f0; border: 1px solid #1a2035; border-radius: 6px; padding: 6px 8px; }
  .btn-replay { background: #7c3aed; color: #fff; border: 0; border-radius: 6px; padding: 6px 14px; font-weight: 700; cursor: pointer; }
  .btn-replay:disabled { opacity: 0.5; cursor: default; }
  .replay-out { margin-top: 4px; }
  .hint { color: #94a3b8; background: #11182f; border: 1px solid #1a2035; border-radius: 8px; padding: 12px; }
  .hint code { color: #06b6d4; }

  .board { display: flex; flex-direction: column; gap: 5px; margin-bottom: 28px; }
  .brow { display: grid; grid-template-columns: 40px 1fr 90px 90px; align-items: center; gap: 10px;
    background: #0a0e1a; border: 1px solid #1a2035; border-radius: 8px; padding: 9px 14px; }
  .rank { color: #7c3aed; font-weight: 700; font-family: 'Geist Mono', monospace; }
  .buser { color: #e2e8f0; font-weight: 600; }
  .breq { color: #64748b; text-align: right; font-family: 'Geist Mono', monospace; font-size: 12px; }
  .bsaved { color: #10b981; text-align: right; font-weight: 700; font-family: 'Geist Mono', monospace; }
  .label { font-size: 10px; color: #64748b; text-transform: uppercase; letter-spacing: 0.12em; }

  .providers { display: flex; gap: 8px; flex-wrap: wrap; margin-bottom: 18px; }
  .chip { display: flex; align-items: center; gap: 7px; background: #0a0e1a; border: 1px solid #1a2035; border-radius: 999px; padding: 6px 13px; font-size: 12px; }
  .chip .dot { width: 7px; height: 7px; }
  .chip.up .dot { color: #10b981; } .chip.down .dot { color: #ef4444; }
  .chip .tier { color: #64748b; font-size: 10px; text-transform: uppercase; letter-spacing: 0.1em; }

  .mix-panel { margin-bottom: 14px; }
  .mixbar { display: flex; height: 14px; border-radius: 7px; overflow: hidden; background: #11182f; }
  .seg { height: 100%; transition: width 0.4s ease; }
  .seg:not(:last-child) { border-right: 1px solid #0a0e1a; }
  .mixlegend { display: flex; gap: 16px; flex-wrap: wrap; margin-top: 10px; }
  .mleg { display: flex; align-items: center; gap: 6px; font-size: 11px; color: #94a3b8; text-transform: capitalize; }
  .mleg b { color: #e2e8f0; font-family: 'Geist Mono', monospace; }
  .mdot { width: 9px; height: 9px; border-radius: 2px; }

  .charts { display: grid; grid-template-columns: 1.4fr 1fr; gap: 14px; margin-bottom: 22px; }
  .panel { background: #0a0e1a; border: 1px solid #1a2035; border-radius: 8px; padding: 16px 18px; }
  .panel-title { font-size: 10px; color: #64748b; text-transform: uppercase; letter-spacing: 0.12em; margin-bottom: 12px; }
  .canvas-wrap { height: 200px; position: relative; }

  .feed-title { font-size: 11px; color: #64748b; text-transform: uppercase; letter-spacing: 0.12em; margin-bottom: 10px; }
  .feed { display: flex; flex-direction: column; gap: 5px; }
  .empty { color: #64748b; padding: 40px; text-align: center; }

  .row { display: grid; grid-template-columns: 90px 160px 90px 1fr 90px 70px 48px; gap: 14px; align-items: center; font-family: 'Geist Mono', monospace; background: #0a0e1a; border: 1px solid #1a2035; border-radius: 6px; padding: 9px 15px; animation: slideIn 0.2s ease; }
  @keyframes slideIn { from { opacity: 0; transform: translateY(-8px); } to { opacity: 1; transform: none; } }
  .provider { color: #06b6d4; font-weight: 600; }
  .model { color: #64748b; font-size: 11px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .tokens { color: #94a3b8; } .cost { color: #10b981; } .latency { color: #64748b; }
  .code { text-align: right; color: #64748b; } .code.err { color: #ef4444; }
  .cx { font-size: 10px; padding: 2px 8px; border-radius: 4px; text-transform: uppercase; letter-spacing: 0.08em; text-align: center; }
  .cx.simple { background: rgba(16,185,129,0.12); color: #10b981; }
  .cx.standard { background: rgba(6,182,212,0.12); color: #06b6d4; }
  .cx.complex { background: rgba(124,58,237,0.14); color: #7c3aed; }
  .cx.critical { background: rgba(239,68,68,0.12); color: #ef4444; }

  /* ── Setup wizard ── */
  .setup-bg { position: fixed; inset: 0; background: radial-gradient(circle at 30% 20%, rgba(124,58,237,0.10), transparent 60%), #050816; z-index: 100; display: flex; align-items: center; justify-content: center; padding: 24px; overflow-y: auto; }
  .setup-card { background: #0a0e1a; border: 1px solid #1a2035; border-radius: 14px; width: 100%; max-width: 620px; padding: 28px 32px; box-shadow: 0 30px 80px -20px rgba(124,58,237,0.25); }
  .setup-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 22px; }
  .setup-logo { font-family: 'Inter',sans-serif; font-weight: 800; font-size: 22px; letter-spacing: -0.04em; color: #7c3aed; }
  .setup-logo span { color: #06b6d4; }
  .setup-steps { display: flex; gap: 8px; }
  .setup-steps .dot { width: 9px; height: 9px; border-radius: 50%; background: #1a2035; transition: background 0.2s; }
  .setup-steps .dot.on { background: #7c3aed; box-shadow: 0 0 8px #7c3aed; }
  .setup-steps .dot.done { background: #10b981; }
  .skip { background: transparent; color: #64748b; border: 0; cursor: pointer; font-size: 12px; }
  .skip:hover { color: #e2e8f0; }
  .setup-card h2 { font-size: 22px; font-weight: 700; letter-spacing: -0.02em; color: #e2e8f0; margin-bottom: 8px; }
  .setup-card .lead { color: #94a3b8; font-size: 13px; line-height: 1.55; margin-bottom: 18px; }
  .setup-card .lead b { color: #e2e8f0; }
  .bullets { list-style: none; padding: 0; margin: 0 0 22px; display: flex; flex-direction: column; gap: 7px; }
  .bullets li { color: #cbd5e1; font-size: 13px; padding: 9px 12px; background: #11182f; border: 1px solid #1a2035; border-radius: 7px; }
  .setup-actions { display: flex; align-items: center; justify-content: space-between; gap: 8px; margin-top: 22px; }
  .setup-cta { background: #7c3aed; color: #fff; border: 0; border-radius: 7px; padding: 9px 18px; font-weight: 700; cursor: pointer; font-size: 13px; }
  .setup-cta:hover { background: #6d28d9; }
  .setup-cta:disabled { opacity: 0.5; cursor: default; }
  .setup-ghost { background: transparent; color: #94a3b8; border: 1px solid #1a2035; border-radius: 7px; padding: 8px 14px; cursor: pointer; font-size: 12px; }
  .setup-ghost:hover { color: #e2e8f0; border-color: #2a2150; }
  .setup-ghost.small { padding: 5px 11px; font-size: 11px; }
  .prov-list { display: flex; flex-direction: column; gap: 7px; margin-bottom: 4px; }
  .prov-row { display: grid; grid-template-columns: 110px 1fr auto; align-items: center; gap: 10px; padding: 9px 12px; background: #11182f; border: 1px solid #1a2035; border-radius: 7px; }
  .prov-row.test { grid-template-columns: 110px 1fr 60px; }
  .prov-name { font-weight: 600; color: #06b6d4; font-size: 13px; text-transform: capitalize; }
  .prov-env { color: #10b981; font-size: 12px; }
  .prov-input { background: #050816; color: #e2e8f0; border: 1px solid #1a2035; border-radius: 5px; padding: 6px 9px; font-size: 12px; font-family: 'Geist Mono', monospace; }
  .prov-input:focus { outline: none; border-color: #7c3aed; }
  .test-status .ok { color: #10b981; font-size: 12px; font-weight: 600; }
  .test-status .err { color: #ef4444; font-size: 11px; }
  .test-status .muted { color: #64748b; font-size: 12px; }
  .codeblock { position: relative; background: #050816; border: 1px solid #1a2035; border-radius: 8px; padding: 12px 14px; margin-bottom: 14px; }
  .codeblock pre { font-family: 'Geist Mono', monospace; font-size: 12px; color: #cbd5e1; white-space: pre-wrap; line-height: 1.6; margin: 0; }
  .copy-btn { position: absolute; top: 8px; right: 8px; background: #1a2035; color: #94a3b8; border: 0; border-radius: 5px; padding: 4px 9px; font-size: 11px; cursor: pointer; }
  .copy-btn:hover { background: #2a2150; color: #e2e8f0; }
  .hint { color: #94a3b8; font-size: 12px; line-height: 1.55; padding: 10px 12px; background: #11182f; border: 1px solid #1a2035; border-radius: 7px; }
  .hint code { color: #06b6d4; }
  .setup-done { text-align: center; padding: 60px 0; font-size: 20px; color: #10b981; font-weight: 700; }

  /* ── Playground ── */
  .header-right { display: flex; align-items: center; gap: 16px; }
  .pg-open { background: linear-gradient(90deg, #7c3aed, #06b6d4); color: #fff; border: 0; border-radius: 7px; padding: 7px 14px; font-size: 12px; font-weight: 700; cursor: pointer; letter-spacing: 0.02em; }
  .pg-open:hover { filter: brightness(1.1); }
  .pg-bg { position: fixed; inset: 0; background: rgba(0,0,0,0.7); display: flex; align-items: center; justify-content: center; padding: 24px; z-index: 90; }
  .pg-card { background: #0a0e1a; border: 1px solid #1a2035; border-radius: 12px; width: 100%; max-width: 880px; height: 78vh; display: flex; flex-direction: column; box-shadow: 0 30px 80px -20px rgba(124,58,237,0.3); }
  .pg-head { display: flex; align-items: center; justify-content: space-between; padding: 14px 18px; border-bottom: 1px solid #1a2035; gap: 10px; flex-wrap: wrap; }
  .pg-title { font-size: 13px; color: #e2e8f0; }
  .pg-title .muted { color: #64748b; margin-left: 6px; }
  .pg-controls { display: flex; align-items: center; gap: 8px; }
  .pg-select { background: #050816; color: #e2e8f0; border: 1px solid #1a2035; border-radius: 6px; padding: 6px 10px; font-size: 12px; font-family: 'Geist Mono', monospace; min-width: 230px; }
  .pg-select:focus { outline: none; border-color: #7c3aed; }
  .pg-ghost { background: transparent; color: #94a3b8; border: 1px solid #1a2035; border-radius: 6px; padding: 6px 11px; cursor: pointer; font-size: 12px; }
  .pg-ghost:hover { color: #e2e8f0; border-color: #2a2150; }
  .pg-ghost.small { padding: 5px 10px; font-size: 11px; }
  .pg-feed { flex: 1; overflow-y: auto; padding: 16px 18px; display: flex; flex-direction: column; gap: 12px; }
  .pg-empty { text-align: center; color: #64748b; padding: 50px 24px; }
  .pg-empty-title { font-size: 15px; color: #94a3b8; margin-bottom: 6px; font-weight: 600; }
  .pg-empty-sub { font-size: 12px; line-height: 1.55; max-width: 460px; margin: 0 auto; }
  .pg-turn { display: flex; flex-direction: column; gap: 4px; }
  .pg-turn .pg-who { font-size: 10px; text-transform: uppercase; letter-spacing: 0.1em; color: #64748b; }
  .pg-turn.user .pg-who { color: #06b6d4; }
  .pg-turn.assistant .pg-who { color: #7c3aed; }
  .pg-body { background: #11182f; border: 1px solid #1a2035; border-radius: 8px; padding: 10px 13px; color: #e2e8f0; font-size: 13px; line-height: 1.55; white-space: pre-wrap; word-break: break-word; }
  .pg-turn.user .pg-body { background: #050816; }
  .pg-err { color: #ef4444; font-size: 12px; padding: 8px 12px; background: rgba(239,68,68,0.08); border: 1px solid rgba(239,68,68,0.25); border-radius: 7px; }
  .pg-input { display: grid; grid-template-columns: 1fr auto; gap: 10px; padding: 14px 18px; border-top: 1px solid #1a2035; }
  .pg-input textarea { background: #050816; color: #e2e8f0; border: 1px solid #1a2035; border-radius: 7px; padding: 9px 12px; font-size: 13px; font-family: 'Inter', sans-serif; resize: none; line-height: 1.5; }
  .pg-input textarea:focus { outline: none; border-color: #7c3aed; }
  .pg-input textarea:disabled { opacity: 0.5; }
  .pg-send { background: #7c3aed; color: #fff; border: 0; border-radius: 7px; padding: 0 18px; font-size: 13px; font-weight: 700; cursor: pointer; min-width: 100px; }
  .pg-send:hover { background: #6d28d9; }
  .pg-send:disabled { opacity: 0.5; cursor: default; }

  /* ── Usage windows panel ── */
  .usage-panel { margin-bottom: 22px; }
  .usage-head { display: flex; align-items: center; justify-content: space-between; gap: 10px; margin-bottom: 12px; }
  .usage-head .panel-title { margin-bottom: 0; }
  .usage-refresh { background: transparent; color: #94a3b8; border: 1px solid #1a2035; border-radius: 6px; padding: 5px 11px; font-size: 11px; cursor: pointer; }
  .usage-refresh:hover { color: #e2e8f0; border-color: #2a2150; }
  .usage-refresh:disabled { opacity: 0.5; cursor: default; }

  .uw-cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 10px; margin-bottom: 14px; }
  .uw-card { display: flex; flex-direction: column; gap: 7px; background: #11182f; border: 1px solid #1a2035; border-radius: 7px; padding: 10px 12px; }
  .uw-card-head { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; }
  .uw-prov { color: #06b6d4; font-weight: 600; }
  .uw-card-head .muted { color: #64748b; font-size: 11px; }
  .uw-stats { display: flex; flex-wrap: wrap; gap: 5px 14px; font-family: 'Geist Mono', monospace; font-size: 12px; color: #64748b; }
  .uw-stats b { color: #e2e8f0; font-weight: 600; }
  .uw-meta { display: flex; gap: 14px; font-family: 'Geist Mono', monospace; font-size: 11px; color: #64748b; }
  .uw-meta b { color: #06b6d4; font-weight: 600; }
  .uw-quota { display: flex; flex-direction: column; gap: 4px; }
  .uw-quota-label { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; font-size: 11px; }
  .uw-quota-label .muted { color: #64748b; }
  .uw-quota-label b { font-family: 'Geist Mono', monospace; color: #e2e8f0; }
  .uw-bar { height: 6px; border-radius: 3px; background: #050816; overflow: hidden; }
  .uw-fill { height: 100%; border-radius: 3px; transition: width 0.3s ease; }
  .uw-reset { font-size: 11px; color: #64748b; }
  .uw-noquota { font-size: 11px; color: #64748b; }

  .uw-rows { display: flex; flex-direction: column; gap: 5px; }
  .uw-row { display: grid; grid-template-columns: 84px minmax(0, 1fr) 88px 40px 60px; gap: 10px; align-items: center; background: #11182f; border: 1px solid #1a2035; border-radius: 6px; padding: 8px 13px; font-family: 'Geist Mono', monospace; font-size: 12px; }
  .uw-row:hover { border-color: #2a2150; }
  .uw-row.active { border-color: #7c3aed; }
  .uw-r-prov { color: #06b6d4; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .uw-r-time { color: #94a3b8; font-size: 11px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .uw-r-req { color: #64748b; text-align: right; }
  .uw-r-tok { color: #94a3b8; text-align: right; }

  .erc { font-size: 9px; padding: 2px 8px; border-radius: 4px; text-transform: uppercase; letter-spacing: 0.08em; text-align: center; white-space: nowrap; }
  .erc.open { background: rgba(16,185,129,0.12); color: #10b981; }
  .erc.limit { background: rgba(239,68,68,0.12); color: #ef4444; }
  .erc.reset { background: rgba(6,182,212,0.12); color: #06b6d4; }
  .erc.elapsed, .erc.unknown { background: rgba(100,116,139,0.14); color: #94a3b8; }

  .uw-drill { margin-top: 12px; background: #11182f; border: 1px solid #1a2035; border-radius: 8px; padding: 10px 12px; }
  .uw-drill-head { display: flex; align-items: center; justify-content: space-between; gap: 8px; margin-bottom: 8px; font-size: 10px; color: #64748b; text-transform: uppercase; letter-spacing: 0.12em; }
  .uw-drill-head .x { width: 22px; height: 22px; font-size: 11px; }
  .uw-drill-note { font-size: 12px; padding: 4px 2px; color: #64748b; }
  .uw-drill-note.err { color: #ef4444; }
  .uw-ev { display: grid; grid-template-columns: 56px minmax(0, 1fr) 42px 54px 54px 54px minmax(88px, auto); gap: 10px; align-items: center; background: #0a0e1a; border: 1px solid #1a2035; border-radius: 6px; padding: 6px 12px; font-family: 'Geist Mono', monospace; font-size: 11px; margin-bottom: 4px; }
  .uw-ev-head { background: transparent; border: 0; padding: 0 12px; margin-bottom: 2px; color: #64748b; font-size: 9px; text-transform: uppercase; letter-spacing: 0.1em; }
  .uw-ev-time { color: #64748b; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .uw-ev-model { color: #94a3b8; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .uw-ev-status { text-align: center; }
  .uw-ev-status.ok { color: #10b981; }
  .uw-ev-status.bad { color: #ef4444; }
  .uw-ev-tok { text-align: right; color: #94a3b8; white-space: nowrap; }
  .uw-ev-tok.nil { color: #64748b; }
  .uw-ev-badges { display: flex; flex-wrap: wrap; gap: 4px; justify-content: flex-end; }
  .evb { font-size: 9px; padding: 1px 6px; border-radius: 4px; text-transform: uppercase; letter-spacing: 0.06em; }
  .evb.probe { background: rgba(100,116,139,0.14); color: #94a3b8; }
  .evb.limit { background: rgba(239,68,68,0.12); color: #ef4444; }
  .evb.partial { background: rgba(245,158,11,0.12); color: #f59e0b; }

  @media (max-width: 620px) {
    .uw-row { grid-template-columns: 72px minmax(0, 1fr) 88px 36px 54px; gap: 8px; padding: 8px 10px; }
    .uw-ev, .uw-ev-head { grid-template-columns: 44px minmax(0, 1fr) 34px 44px 44px minmax(76px, auto); gap: 8px; }
    .uw-ev-cache { display: none; }
  }
</style>
