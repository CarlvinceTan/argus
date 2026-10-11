<script lang="ts">
  import FindingCard from "./components/FindingCard.svelte";
  import LaunchPanel from "./components/LaunchPanel.svelte";
  import { api, setToken } from "./lib/api";
  import RunSidebar from "./components/RunSidebar.svelte";
  import { formatCost, formatDuration, groupFindings, severityRank } from "./lib/format";
  import { SEVERITY_ORDER, type RunDetail, type RunSummary } from "./lib/types";

  let runs = $state<RunSummary[]>([]);
  let roots = $state<string[]>([]);
  let root = $state("");
  let selected = $state("");
  let detail = $state<RunDetail | null>(null);
  let error = $state("");
  let sortMode = $state<"severity" | "probability" | "group">("severity");
  let groupMode = $state<"none" | "page" | "feature">("none");
  let severityFilter = $state<Set<string>>(new Set());
  let search = $state("");
  let showBelow = $state(false);
  let showCandidates = $state(false);
  let verdicts = $state<Record<string, string>>({});
  let learning = $state<any>(null);
  let showLearning = $state(false);

  async function loadRuns(target: string) {
    error = "";
    try {
      const found = (await api.runs(target)) as RunSummary[];
      runs = found ?? [];
      if (!selected && runs.length > 0) {
        await selectRun(runs[0].path);
      }
    } catch (exc) {
      error = `could not list runs: ${exc}`;
    }
  }

  async function selectRun(path: string) {
    selected = path;
    error = "";
    try {
      detail = (await api.run(path)) as RunDetail;
      verdicts = ((await api.labels(path)) as Record<string, string>) ?? {};
    } catch (exc) {
      detail = null;
      error = `could not load run: ${exc}`;
    }
  }

  async function recordVerdict(findingId: string, verdict: string, note: string) {
    if (!detail) return;
    try {
      learning = await api.label({ runPath: selected, findingId, verdict, note, reviewer: "" });
      verdicts = { ...verdicts, [findingId]: verdict };
      showLearning = true;
    } catch (exc) {
      if (String(exc).includes("unauthorized") || (exc as { status?: number }).status === 401) {
        const entered = window.prompt("This console needs an access token for actions:");
        if (entered) {
          setToken(entered);
          return recordVerdict(findingId, verdict, note);
        }
      }
      error = `could not record the verdict: ${exc}`;
    }
  }

  async function recalibrate() {
    try {
      learning = await api.recalibrate();
    } catch (exc) {
      error = `could not recalibrate: ${exc}`;
    }
  }

  async function loadLearning() {
    try {
      learning = await api.calibration();
    } catch {
      learning = null;
    }
  }

  async function boot() {
    try {
      roots = (await api.roots()) ?? [];
      root = roots[0] ?? "";
    } catch {
      roots = [];
    }
    await loadRuns(root);
    await loadLearning();
  }

  boot();

  const reported = $derived(detail?.findings.filter((finding) => finding.reported) ?? []);

  const visible = $derived.by(() => {
    let list = reported;
    if (severityFilter.size > 0) {
      list = list.filter((finding) => severityFilter.has(finding.severity_name));
    }
    if (search.trim()) {
      const needle = search.trim().toLowerCase();
      list = list.filter((finding) =>
        `${finding.title} ${finding.brief} ${finding.group} ${finding.screen} ${finding.feature ?? ""}`
          .toLowerCase()
          .includes(needle),
      );
    }
    const sorted = [...list];
    switch (sortMode) {
      case "probability":
        sorted.sort((a, b) => b.defect_prob - a.defect_prob);
        break;
      case "group":
        sorted.sort((a, b) => a.group.localeCompare(b.group) || severityRank(a.severity_name) - severityRank(b.severity_name));
        break;
      default:
        sorted.sort(
          (a, b) => severityRank(a.severity_name) - severityRank(b.severity_name) || b.defect_prob - a.defect_prob,
        );
    }
    return sorted;
  });

  const buckets = $derived(groupMode === "none" ? null : groupFindings(visible, groupMode));

  function toggleSeverity(name: string) {
    const next = new Set(severityFilter);
    if (next.has(name)) next.delete(name);
    else next.add(name);
    severityFilter = next;
  }

  function severityCounts(): { name: string; count: number }[] {
    return SEVERITY_ORDER.map((name) => ({
      name,
      count: reported.filter((finding) => finding.severity_name === name).length,
    }));
  }
</script>

<div class="app">
  <RunSidebar
    {runs}
    {roots}
    {selected}
    onSelect={selectRun}
    onRoot={(target) => {
      root = target;
      selected = "";
      detail = null;
      loadRuns(target);
    }}
  />

  <main>
    {#if error}
      <p class="error">{error}</p>
    {/if}

    {#if detail}
      <header class="top">
        <div>
          <h2>{detail.summary.subject}</h2>
          <p class="sub">
            <span>{detail.summary.mode}</span>
            <span>{detail.summary.decisionModel}</span>
            {#if detail.summary.visionModel}<span>{detail.summary.visionModel}</span>{/if}
            <span>{detail.summary.terminal}</span>
            <span>{formatDuration(detail.summary.durationMs)}</span>
            <span>{detail.observations} observations</span>
            <span>{detail.frameCount} frames</span>
            <span>{detail.decisionCalls} decision calls</span>
            {#if detail.visionCalls}<span>{detail.visionCalls} vision calls</span>{/if}
            <span>{formatCost(detail.summary.costUsd)}</span>
          </p>
        </div>
        <div class="isolation">
          {#if detail.isolation && Object.keys(detail.isolation).length}
            <span class="badge ok" title={JSON.stringify(detail.isolation)}>isolated</span>
          {/if}
          {#if detail.sessionClip}
            <a class="badge" href={detail.sessionClip} target="_blank" rel="noreferrer">session video</a>
          {/if}
        </div>
      </header>

      {#if detail.notes.length}
        <ul class="notes">
          {#each detail.notes as note}<li>{note}</li>{/each}
        </ul>
      {/if}

      <LaunchPanel onFinished={() => loadRuns(root)} />

      <div class="controls">
        <div class="filter">
          {#each severityCounts() as item}
            {#if item.count > 0 || severityFilter.has(item.name)}
              <button
                class:active={severityFilter.has(item.name)}
                onclick={() => toggleSeverity(item.name)}
              >
                {item.name} {item.count}
              </button>
            {/if}
          {/each}
        </div>
        <label>
          sort
          <select bind:value={sortMode}>
            <option value="severity">by severity</option>
            <option value="probability">by probability</option>
            <option value="group">by page</option>
          </select>
        </label>
        <label>
          group
          <select bind:value={groupMode}>
            <option value="none">flat backlog</option>
            <option value="page">by page</option>
            <option value="feature">by feature</option>
          </select>
        </label>
        <input class="search" placeholder="filter findings" bind:value={search} />
        <button class="toggle" class:active={showBelow} onclick={() => (showBelow = !showBelow)}>
          below threshold ({detail.findings.length - reported.length})
        </button>
        <button class="toggle" class:active={showCandidates} onclick={() => (showCandidates = !showCandidates)}>
          candidates ({detail.candidates.length})
        </button>
        <button class="toggle" class:active={showLearning} onclick={() => (showLearning = !showLearning)}>
          learning {learning?.labels ? `(${learning.labels})` : ""}
        </button>
      </div>

      {#if showLearning}
        <section class="learning">
          <h3>
            Learning from review
            <span>{learning?.summary ?? "no calibration yet"}</span>
          </h3>
          <div class="learn-grid">
            <div>
              <h4>verdicts recorded</h4>
              {#if learning?.history?.by_verdict}
                {#each Object.entries(learning.history.by_verdict) as [name, count]}
                  <div class="row"><span>{name}</span><b>{count}</b></div>
                {/each}
              {:else}
                <p class="empty">no labels yet</p>
              {/if}
              <button class="toggle" onclick={recalibrate}>rebuild calibration</button>
            </div>
            <div>
              <h4>check precision</h4>
              {#each Object.entries(learning?.checks ?? {}).sort((a: any, b: any) => b[1].samples - a[1].samples) as [key, stat]: any}
                <div class="row">
                  <span>{key}</span>
                  <b>{stat.precision.toFixed(2)} <small>n={stat.samples}</small></b>
                </div>
              {/each}
              {#if learning?.suppressed?.length}
                <p class="flag">suppressed: {learning.suppressed.join(", ")}</p>
              {/if}
              {#if learning?.promoted?.length}
                <p class="flag good">promoted: {learning.promoted.join(", ")}</p>
              {/if}
            </div>
          </div>
          <p class="hint">
            Verdicts and notes are appended to {learning?.labelPath ?? "the label log"}. Rebuilding writes
            calibration data the pipeline reads on the next run; deleting it restores stock behaviour.
            Misclassifications still count as real issues, so a wrong class never teaches the system to stay quiet.
          </p>
        </section>
      {/if}

      {#if buckets}
        {#each buckets as bucket}
          <section class="bucket">
            <h3>
              {bucket.key}
              <span>{bucket.findings.length} findings · worst {bucket.worst}</span>
            </h3>
            {#each bucket.findings as finding (finding.id)}
              <FindingCard
                {finding}
                terminal={detail.summary.terminal}
                verdict={verdicts[finding.id] ?? ""}
                onVerdict={recordVerdict}
              />
            {/each}
          </section>
        {/each}
      {:else}
        {#each visible as finding (finding.id)}
          <FindingCard
            {finding}
            terminal={detail.summary.terminal}
            verdict={verdicts[finding.id] ?? ""}
            onVerdict={recordVerdict}
          />
        {/each}
      {/if}

      {#if visible.length === 0}
        <p class="empty">Nothing meets the reporting threshold in this run.</p>
      {/if}

      {#if showBelow}
        <section class="bucket">
          <h3>Below the reporting threshold <span>{detail.findings.length - reported.length}</span></h3>
          {#each detail.findings.filter((finding) => !finding.reported) as finding (finding.id)}
            <FindingCard {finding} terminal={detail.summary.terminal} showOverlay={false} />
          {/each}
        </section>
      {/if}

      {#if showCandidates}
        <section class="bucket">
          <h3>Candidates considered <span>{detail.candidates.length}</span></h3>
          <table class="candidates">
            <thead>
              <tr><th>obs</th><th>screen</th><th>action</th><th>check</th><th>region</th><th>verdict</th><th>detail</th></tr>
            </thead>
            <tbody>
              {#each detail.candidates as candidate}
                <tr>
                  <td>{candidate.observation}</td>
                  <td>{candidate.screen}</td>
                  <td>{candidate.action}</td>
                  <td>{candidate.check}</td>
                  <td>{candidate.region}</td>
                  <td>{candidate.verdict}</td>
                  <td class="detail">{candidate.detail}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </section>

        <section class="bucket">
          <h3>Rejected findings <span>{detail.rejected.length}</span></h3>
          {#each detail.rejected as finding (finding.id)}
            <FindingCard {finding} terminal={detail.summary.terminal} showOverlay={false} />
          {/each}
        </section>
      {/if}

      <details class="log">
        <summary>run log ({detail.log.length})</summary>
        <pre>{detail.log.join("\n")}</pre>
      </details>
    {:else}
      <p class="empty">Select a run to inspect its backlog.</p>
    {/if}
  </main>
</div>

<style>
  :global(body) {
    margin: 0;
    background: #0b0c10;
    color: #e8edf7;
    font-family: "Inter", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    font-size: 13px;
  }
  .app { display: flex; height: 100vh; overflow: hidden; }
  main { flex: 1; overflow-y: auto; padding: 20px 24px 60px; }
  .top { display: flex; justify-content: space-between; align-items: flex-start; gap: 16px; }
  h2 { margin: 0 0 4px; font-size: 19px; }
  .sub { margin: 0; display: flex; gap: 12px; flex-wrap: wrap; font-size: 11.5px; color: #8b93a7; }
  .isolation { display: flex; gap: 8px; align-items: center; }
  .badge {
    font-size: 11px;
    border: 1px solid #262b38;
    border-radius: 999px;
    padding: 3px 10px;
    color: #9aa4b8;
    text-decoration: none;
  }
  .badge.ok { color: #4ade80; border-color: #1f4d33; }
  .notes { margin: 12px 0 0; padding-left: 18px; color: #8b93a7; font-size: 12px; }
  .controls {
    display: flex;
    gap: 12px;
    align-items: center;
    flex-wrap: wrap;
    margin: 18px 0 12px;
    padding: 10px 12px;
    background: #101319;
    border: 1px solid #1c1f2a;
    border-radius: 10px;
  }
  .filter { display: flex; gap: 6px; }
  .filter button, .toggle {
    background: #161a23;
    border: 1px solid #232734;
    color: #9aa4b8;
    border-radius: 6px;
    font-size: 11px;
    padding: 4px 10px;
    cursor: pointer;
  }
  .filter button.active, .toggle.active { color: #e8edf7; border-color: #3a4260; background: #232838; }
  label { font-size: 11px; color: #8b93a7; display: flex; gap: 6px; align-items: center; }
  select, .search {
    background: #161a23;
    border: 1px solid #232734;
    color: #dfe5f0;
    border-radius: 6px;
    padding: 4px 8px;
    font-size: 11.5px;
  }
  .search { flex: 1; min-width: 140px; }
  .bucket { margin: 22px 0; display: flex; flex-direction: column; gap: 10px; }
  .bucket h3 { margin: 0 0 4px; font-size: 13px; display: flex; gap: 10px; align-items: baseline; }
  .bucket h3 span { font-size: 11px; color: #6f7789; font-weight: 400; }
  .empty { color: #6f7789; }
  .error { color: #ff8080; }
  .candidates { width: 100%; border-collapse: collapse; font-size: 11px; }
  .candidates th, .candidates td { text-align: left; padding: 5px 8px; border-bottom: 1px solid #1c1f2a; vertical-align: top; }
  .candidates th { color: #6f7789; font-weight: 600; }
  .candidates td.detail { color: #8b93a7; }
  .learning {
    margin: 18px 0;
    padding: 14px 16px;
    background: #101319;
    border: 1px solid #1c1f2a;
    border-radius: 10px;
  }
  .learning h3 { margin: 0 0 10px; font-size: 13px; display: flex; gap: 10px; align-items: baseline; }
  .learning h3 span { font-size: 11px; color: #6f7789; font-weight: 400; }
  .learn-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 18px; }
  .learning h4 { margin: 0 0 6px; font-size: 11px; text-transform: uppercase; letter-spacing: 0.06em; color: #6f7789; }
  .learning .row { display: flex; justify-content: space-between; font-size: 11.5px; padding: 1px 0; color: #9aa4b8; }
  .learning .row b { color: #cdd5e4; font-weight: 600; }
  .learning .row small { color: #6f7789; font-weight: 400; }
  .learning .flag { font-size: 11px; color: #ffb4b4; margin: 6px 0 0; }
  .learning .flag.good { color: #8ce99a; }
  .learning .hint { margin: 12px 0 0; font-size: 11px; color: #6f7789; line-height: 1.5; }
  .log { margin-top: 24px; color: #8b93a7; font-size: 11.5px; }
  .log pre { max-height: 320px; overflow: auto; background: #101319; padding: 10px; border-radius: 8px; }
</style>
