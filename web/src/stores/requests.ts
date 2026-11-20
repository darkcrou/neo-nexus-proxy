import { writable, derived } from 'svelte/store'

export interface Request {
  id: number
  provider: string
  model_asked: string
  model_used: string
  complexity: string
  input_tokens: number
  output_tokens: number
  cost_usd: number
  latency_ms: number
  status: number
  timestamp: string
}

export interface Stats {
  total_requests: number
  total_cost_usd: number
  total_tokens: number
  forecast_usd: number
  avg_latency_ms: number
  cache_saved_usd: number
  cache_read_tokens: number
  redacted_total: number
}

export interface ProviderStatus {
  name: string
  tier: string
  healthy: boolean
  // Enriched fields from /api/providers (Providers tab). Optional so the
  // overview chips keep working against the legacy shape.
  family?: string
  models?: string[]
  model_source?: 'live' | 'configured' | 'defaults'
  discovery?: 'ok' | 'pending' | 'unsupported' | 'failed' | 'off' | 'not_loaded'
  base_url?: string
  keys?: number
  key_source?: 'config' | 'env' | 'none'
  key_env?: string
}

export interface TimeBucket {
  bucket: string
  requests: number
  cost_usd: number
}

export interface Savings {
  period: string
  requests: number
  actual_usd: number
  baseline_usd: number
  saved_usd: number
  percent_saved: number
  cache_saved_usd: number
}

// Stores
export const requests = writable<Request[]>([])
export const stats = writable<Stats>({
  total_requests: 0,
  total_cost_usd: 0,
  total_tokens: 0,
  forecast_usd: 0,
  avg_latency_ms: 0,
  cache_saved_usd: 0,
  cache_read_tokens: 0,
  redacted_total: 0,
})
export const providers = writable<ProviderStatus[]>([])
export const timeseries = writable<TimeBucket[]>([])
export const savings = writable<Savings>({
  period: 'month', requests: 0, actual_usd: 0, baseline_usd: 0, saved_usd: 0, percent_saved: 0, cache_saved_usd: 0,
})
export const connected = writable(false)

// Derived: last 100 requests
export const recentRequests = derived(requests, $r => $r.slice(0, 100))

// Derived: routing mix by complexity tier, from the recent request window.
// Shows how the classifier split traffic (simple→free, standard→cheap, …).
export const complexityMix = derived(requests, $r => {
  const order = ['simple', 'standard', 'complex', 'critical']
  const counts: Record<string, number> = { simple: 0, standard: 0, complex: 0, critical: 0 }
  for (const req of $r) {
    if (req.complexity in counts) counts[req.complexity]++
  }
  const total = order.reduce((n, k) => n + counts[k], 0) || 1
  return order.map(key => ({ key, count: counts[key], pct: (counts[key] / total) * 100 }))
})

// Derived: provider breakdown
export const providerBreakdown = derived(requests, $r => {
  const map: Record<string, { count: number; cost: number; tokens: number }> = {}
  for (const req of $r) {
    if (!map[req.provider]) map[req.provider] = { count: 0, cost: 0, tokens: 0 }
    map[req.provider].count++
    map[req.provider].cost += req.cost_usd
    map[req.provider].tokens += req.input_tokens + req.output_tokens
  }
  return map
})

// Load the current state from the REST API so the dashboard isn't empty
// before the first SSE event arrives.
export async function fetchInitial(baseURL = '') {
  try {
    const s = await (await fetch(`${baseURL}/api/stats`)).json()
    stats.set(s as Stats)
  } catch {}
  try {
    const d = await (await fetch(`${baseURL}/api/requests`)).json()
    requests.set((d.requests ?? []) as Request[])
  } catch {}
  try {
    const p = await (await fetch(`${baseURL}/api/providers`)).json()
    providers.set((p.providers ?? []) as ProviderStatus[])
  } catch {}
  await fetchTimeseries(baseURL)
  await fetchSavings(baseURL)
}

// Fetch the cost/requests time series for the chart.
export async function fetchTimeseries(baseURL = '') {
  try {
    const d = await (await fetch(`${baseURL}/api/timeseries`)).json()
    timeseries.set((d.series ?? []) as TimeBucket[])
  } catch {}
}

// Fetch the "saved vs. Claude" headline metric.
export async function fetchSavings(baseURL = '') {
  try {
    savings.set(await (await fetch(`${baseURL}/api/savings?period=month`)).json() as Savings)
  } catch {}
}

// SSE connection
let eventSource: EventSource | null = null

export function connectSSE(baseURL = '') {
  if (eventSource) eventSource.close()

  eventSource = new EventSource(`${baseURL}/events`)
  eventSource.onopen = () => connected.set(true)
  eventSource.onerror = () => {
    connected.set(false)
    setTimeout(() => connectSSE(baseURL), 3000)
  }
  eventSource.onmessage = (e) => {
    try {
      handleEvent(JSON.parse(e.data))
    } catch (err) {
      console.error('SSE parse error:', err)
    }
  }
}

function handleEvent(event: { type: string; data: any }) {
  switch (event.type) {
    case 'connected':
      connected.set(true)
      break
    case 'request':
      requests.update(r => [event.data as Request, ...r].slice(0, 200))
      break
    case 'stats':
      stats.set(event.data as Stats)
      break
  }
}

export function disconnectSSE() {
  eventSource?.close()
  eventSource = null
  connected.set(false)
}
