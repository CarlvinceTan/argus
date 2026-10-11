<script lang="ts">
  import { SEVERITY_COLOURS, SEVERITY_ORDER, type RunSummary } from "../lib/types";
  import { formatClock, formatCost } from "../lib/format";

  interface Props {
    runs: RunSummary[];
    selected: string;
    roots: string[];
    onSelect: (path: string) => void;
    onRoot: (root: string) => void;
  }

  let { runs, selected, roots, onSelect, onRoot }: Props = $props();
</script>

<aside>
  <div class="brand">
    <span class="dot"></span>
    <div>
      <h1>Argus View</h1>
      <p>UI defect backlog</p>
    </div>
  </div>

  {#if roots.length > 1}
    <nav class="roots">
      {#each roots as root}
        <button onclick={() => onRoot(root)} title={root}>{root.split("/").slice(-2).join("/")}</button>
      {/each}
    </nav>
  {/if}

  <div class="runs">
    {#each runs as run}
      <button class="run" class:active={run.path === selected} onclick={() => onSelect(run.path)}>
        <div class="line">
          <strong>{run.subject}</strong>
          <span>{run.reported}</span>
        </div>
        <div class="sub">
          <span>{run.decisionModel.split("/").slice(-1)[0]}</span>
          <span>{run.terminal}</span>
          <span>{formatCost(run.costUsd)}</span>
        </div>
        <div class="sev">
          {#each SEVERITY_ORDER as name}
            {#if run.severities[name]}
              <span style={`background:${SEVERITY_COLOURS[name]}`} title={`${run.severities[name]} ${name}`}>
                {run.severities[name]}
              </span>
            {/if}
          {/each}
          {#if run.rejected}
            <span class="muted" title={`${run.rejected} rejected candidates`}>−{run.rejected}</span>
          {/if}
        </div>
        <div class="when">{formatClock(run.startedAt)}</div>
      </button>
    {/each}
    {#if runs.length === 0}
      <p class="empty">No runs found in this directory.</p>
    {/if}
  </div>
</aside>

<style>
  aside {
    width: 268px;
    min-width: 268px;
    border-right: 1px solid #1c1f2a;
    background: #0d0f14;
    display: flex;
    flex-direction: column;
    height: 100vh;
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 18px 16px 12px;
    border-bottom: 1px solid #1c1f2a;
  }
  .dot {
    width: 10px;
    height: 10px;
    border-radius: 50%;
    background: #6c9aff;
    box-shadow: 0 0 12px #6c9aff88;
  }
  h1 { margin: 0; font-size: 14px; letter-spacing: 0.01em; }
  .brand p { margin: 2px 0 0; font-size: 11px; color: #6f7789; }
  .roots { display: flex; gap: 6px; padding: 10px 12px 0; flex-wrap: wrap; }
  .roots button {
    background: #161a23;
    border: 1px solid #232734;
    color: #9aa4b8;
    border-radius: 6px;
    font-size: 10.5px;
    padding: 3px 8px;
    cursor: pointer;
  }
  .runs { overflow-y: auto; padding: 8px; display: flex; flex-direction: column; gap: 6px; }
  .run {
    text-align: left;
    background: #12151c;
    border: 1px solid #1e222d;
    border-radius: 8px;
    padding: 9px 10px;
    display: flex;
    flex-direction: column;
    gap: 5px;
    cursor: pointer;
    color: inherit;
  }
  .run.active { border-color: #3a4260; background: #161a25; }
  .line { display: flex; justify-content: space-between; align-items: baseline; gap: 8px; }
  .line strong { font-size: 12.5px; }
  .line span { font-size: 12px; color: #ffd60a; font-weight: 700; }
  .sub { display: flex; gap: 8px; font-size: 10.5px; color: #6f7789; }
  .sev { display: flex; gap: 4px; }
  .sev span {
    font-size: 10px;
    color: #0b0c10;
    font-weight: 700;
    border-radius: 4px;
    padding: 1px 5px;
  }
  .sev .muted { background: #262b38; color: #8b93a7; }
  .when { font-size: 10px; color: #5b6274; }
  .empty { color: #6f7789; font-size: 12px; padding: 12px; }
</style>
