<script lang="ts">
  import type { ModelUsage, UsageWindow } from '../stores/usage'

  export let window: UsageWindow

  const numbers = new Intl.NumberFormat()
  const count = (n: number) => numbers.format(n)
  // Reasoning is part of output, so it is not added again.
  const total = (model: ModelUsage) => model.in_tokens + model.cache_read_tokens + model.cache_write_tokens + model.out_tokens
</script>

<div class="summary">
  <h3>Provider totals and model breakdown</h3>
  <span>cache hit <b>{Math.round(window.cache_hit_ratio * 100)}%</b></span>
</div>

<!-- The scroll region is focusable so keyboard users can reach every column. -->
<!-- svelte-ignore a11y-no-noninteractive-tabindex -->
<div class="table-scroll" role="region" aria-label="Provider and model usage table" tabindex="0">
  <table>
    <thead>
      <tr>
        <th scope="col">Model</th>
        <th scope="col">Requests</th>
        <th scope="col" title="Input tokens excluding cache reads and writes">Input</th>
        <th scope="col">Cache read</th>
        <th scope="col">Cache write</th>
        <th scope="col">Output</th>
        <th scope="col" title="Already included in output tokens">Reasoning</th>
        <th scope="col">Total</th>
      </tr>
    </thead>
    <tbody>
      <tr class="provider-total">
        <th scope="row">Provider total</th>
        <td>{count(window.requests)}</td>
        <td>{count(window.in_tokens)}</td>
        <td>{count(window.cache_read_tokens)}</td>
        <td>{count(window.cache_write_tokens)}</td>
        <td>{count(window.out_tokens)}</td>
        <td>{count(window.reasoning_tokens)}</td>
        <td>{count(window.total_tokens)}</td>
      </tr>
      {#each window.per_model ?? [] as model (model.model)}
        <tr>
          <th scope="row">{model.model || 'Unknown model'}</th>
          <td>{count(model.requests)}</td>
          <td>{count(model.in_tokens)}</td>
          <td>{count(model.cache_read_tokens)}</td>
          <td>{count(model.cache_write_tokens)}</td>
          <td>{count(model.out_tokens)}</td>
          <td>{count(model.reasoning_tokens)}</td>
          <td>{count(total(model))}</td>
        </tr>
      {:else}
        <tr><td class="empty" colspan="8">No model breakdown is available for this window.</td></tr>
      {/each}
    </tbody>
  </table>
</div>

<p class="note">Input excludes cache reads and writes. Reasoning is included in output.</p>
{#if window.partial_events > 0}
  <p class="note">Includes {window.partial_events} partially recorded {window.partial_events === 1 ? 'attempt' : 'attempts'}.</p>
{/if}

<style>
  .summary { display: flex; flex-wrap: wrap; align-items: baseline; justify-content: space-between; gap: 6px 12px; margin-bottom: 10px; }
  h3 { margin: 0; font-size: 12px; font-weight: 600; color: #e2e8f0; }
  .summary span { font-size: 11px; color: #64748b; }
  .summary b { color: #06b6d4; }
  .table-scroll { overflow-x: auto; }
  .table-scroll:focus-visible { outline: 2px solid #7c3aed; outline-offset: 2px; }
  table { width: 100%; border-collapse: collapse; font-family: 'Geist Mono', monospace; font-size: 11px; }
  th, td { padding: 9px 10px; text-align: right; white-space: nowrap; border-bottom: 1px solid #1a2035; }
  thead th { font-size: 10px; font-weight: 500; color: #64748b; }
  th:first-child { text-align: left; }
  tbody th { color: #94a3b8; font-weight: 500; }
  td { color: #94a3b8; }
  .provider-total { background: #0a0e1a; }
  .provider-total th, .provider-total td { color: #e2e8f0; font-weight: 600; }
  .empty { text-align: left; color: #64748b; }
  .note { margin: 8px 0 0; font-size: 11px; color: #64748b; }
</style>
