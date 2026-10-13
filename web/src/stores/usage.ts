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

// Stores
export const usageWindows = writable<UsageWindow[]>([])
// Set when the last fetchUsage failed, cleared on success, so the panel can
// say "unavailable" instead of silently staying hidden.
export const usageError = writable('')

// Load the usage panel data: quota windows for every provider with usage.
// (The panel reads window.quota for provider-reported utilization; the
// standalone /api/usage/totals and /api/usage/quota endpoints stay unused
// until a UI needs them.)
export async function fetchUsage(baseURL = '') {
  try {
    const r = await fetch(`${baseURL}/api/usage/windows?limit=20`)
    // res.ok check: the API returns 500 on storage errors — a JSON-bodied
    // failure from a fronting proxy must not be ingested as usage data
    if (!r.ok) throw new Error(`HTTP ${r.status}`)
    const d = await r.json()
    usageWindows.set((d.windows ?? []) as UsageWindow[])
    usageError.set('')
  } catch (e) {
    usageError.set(`usage data unavailable (${e instanceof Error ? e.message : 'network error'}) — refresh to retry`)
  }
}

// Re-fetch everything the usage panel shows.
export async function refreshUsage(baseURL = '') {
  await fetchUsage(baseURL)
}
