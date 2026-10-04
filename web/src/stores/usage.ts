import { writable } from 'svelte/store'

// Usage-measurement stores backed by the /api/usage/* endpoints.
// Unlike the live request feed these are fetched on demand (mount + refresh
// button) — no SSE wiring, no polling.

// Quota utilization snapshot as the provider reported it (transcribed from
// upstream responses, never computed locally).
export interface QuotaObservation {
  dimension: string
  utilization: number // 0..1, exactly as reported
  reset_at?: string
  observed_at: string
}

export interface ModelUsage {
  model: string
  requests: number
  in_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  out_tokens: number
  reasoning_tokens: number // subset of out_tokens — never add to totals
}

// One derived quota window for one provider. ended_at is absent while the
// window is still open; end_reason is "" for open windows.
export interface UsageWindow {
  provider: string
  started_at: string
  ended_at?: string | null
  end_reason: string // "" (open) | TIME_ELAPSED | RATE_LIMIT | PROVIDER_RESET | UNKNOWN
  requests: number
  in_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  out_tokens: number
  reasoning_tokens: number
  total_input_tokens: number // in + cache_read + cache_write
  total_tokens: number // total_input + out
  cache_hit_ratio: number // 0..1
  partial_events: number
  rate_limited_events: number
  quota?: QuotaObservation
  per_model?: ModelUsage[]
}

// A single upstream attempt's measured consumption. Nullable token fields
// mean the provider didn't report them — display as unknown, never as 0.
export interface UsageEvent {
  id: number
  created_at: string
  provider: string
  model_used: string
  model_asked: string
  request_id: string
  key_index: number | null
  attempt: number // 1-based chain position; 0 = probe
  status: number // upstream HTTP status; 0 = transport error
  success: boolean
  stream: boolean
  in_tokens: number | null
  out_tokens: number | null
  cache_read_tokens: number | null
  cache_write_tokens: number | null
  reasoning_tokens: number | null
  usage_partial: boolean
  duration_ms: number
  error: string
  probe: boolean
  rate_limited: boolean
  retry_after: number | null // seconds
  retry_reset_at: string | null
  quota_dimension: string
  quota_utilization: number | null
  quota_reset_at: string | null
  quota_meta: string
}

// Stores
export const usageWindows = writable<UsageWindow[]>([])
export const usageEvents = writable<UsageEvent[]>([])
// Set when the last fetchUsage failed, cleared on success, so the panel can
// say "unavailable" instead of silently staying hidden.
export const usageError = writable('')

// Load the usage panel data: quota windows for every provider with usage.
// (The panel reads window.quota for provider-reported utilization; the
// standalone /api/usage/totals and /api/usage/quota endpoints stay unused
// until a UI needs them.)
export async function fetchUsage(baseURL = '') {
  try {
    const d = await (await fetch(`${baseURL}/api/usage/windows?limit=20`)).json()
    usageWindows.set((d.windows ?? []) as UsageWindow[])
    usageError.set('')
  } catch {
    usageError.set('usage data unavailable — refresh to retry')
  }
}

// Re-fetch everything the usage panel shows.
export async function refreshUsage(baseURL = '') {
  await fetchUsage(baseURL)
}

// Fetch the most recent usage events for one provider (drill-down from a
// window row). Probes stay included so cooldown-recovery pings are visible.
// Returns false when the fetch failed, so the UI can say so honestly.
export async function fetchUsageEvents(provider: string, baseURL = ''): Promise<boolean> {
  usageEvents.set([]) // drop any stale events from a previously selected provider
  try {
    const d = await (await fetch(`${baseURL}/api/usage/events?provider=${encodeURIComponent(provider)}&limit=50`)).json()
    usageEvents.set((d.events ?? []) as UsageEvent[])
    return true
  } catch {
    return false
  }
}
