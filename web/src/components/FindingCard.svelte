<script lang="ts">
  import { SEVERITY_COLOURS, type RunFinding } from "../lib/types";
  import { formatDuration, modelAnswers, regionLabel, regionStyle } from "../lib/format";

  interface Props {
    finding: RunFinding;
    terminal: string;
    /** Percent-style region overlay for the still. */
    showOverlay?: boolean;
    /** Recorded verdict for this finding, if a reviewer has judged it. */
    verdict?: string;
    onVerdict?: (findingId: string, verdict: string, note: string) => Promise<void>;
  }

  let { finding, terminal, showOverlay = true, verdict = "", onVerdict }: Props = $props();
  let tab = $state<"still" | "crop" | "strip" | "clip">("still");
  let showWhy = $state(false);
  let note = $state("");
  let reviewOpen = $state(false);
  let saving = $state(false);

  const VERDICTS = ["accepted", "rejected", "misclassified", "intentional", "duplicate"];

  async function record(value: string) {
    if (!onVerdict) return;
    saving = true;
    try {
      await onVerdict(finding.id, value, note);
      reviewOpen = false;
      note = "";
    } finally {
      saving = false;
    }
  }

  const colour = $derived(SEVERITY_COLOURS[finding.severity_name] ?? "#7c8598");
  const hasClip = $derived(Boolean(finding.evidence.clip));
  const activeSource = $derived(
    tab === "crop" ? finding.evidence.crop
      : tab === "strip" ? finding.evidence.strip
      : tab === "clip" ? finding.evidence.clip
      : finding.evidence.still,
  );
</script>

<article class="card" class:below={!finding.reported}>
  <header>
    <span class="chip" style={`background:${colour}22;color:${colour};border-color:${colour}55`}>
      {finding.severity_name}
    </span>
    <h3>{finding.title}</h3>
    <span class="prob" title="model probability that this is a real defect">
      p={finding.defect_prob.toFixed(2)}
    </span>
    <span class="verified" title="how the finding was confirmed">{finding.verified}</span>
    {#if finding.events > 1}<span class="events">×{finding.events}</span>{/if}
    {#if verdict}
      <span class="verdict" class:good={verdict === "accepted"} title="reviewer verdict">{verdict}</span>
    {/if}
  </header>

  <p class="brief">{finding.brief}</p>

  <div class="meta">
    <span>page <b>{finding.group}</b></span>
    {#if finding.feature}<span>feature <b>{finding.feature}</b></span>{/if}
    <span>screen <b>{finding.screen}</b></span>
    <span>{regionLabel(finding.region)}</span>
    <span>{formatDuration(finding.first_seen_ms)} → {formatDuration(finding.last_seen_ms)}</span>
    {#if finding.tags?.length}<span class="tags">#{finding.tags.join(" #")}</span>{/if}
  </div>

  {#if activeSource || finding.evidence.still}
    <div class="evidence">
      <nav class="tabs">
        <button class:active={tab === "still"} onclick={() => (tab = "still")}>screen</button>
        {#if finding.evidence.crop}<button class:active={tab === "crop"} onclick={() => (tab = "crop")}>crop</button>{/if}
        {#if finding.evidence.strip}<button class:active={tab === "strip"} onclick={() => (tab = "strip")}>frames</button>{/if}
        {#if hasClip}<button class:active={tab === "clip"} onclick={() => (tab = "clip")}>clip</button>{/if}
      </nav>
      <div class="viewport" style={`--terminal:${terminal}`}>
        {#if tab === "clip" && finding.evidence.clip}
          <video src={finding.evidence.clip} controls autoplay loop muted></video>
        {:else if activeSource}
          <img src={activeSource} alt="evidence for {finding.title}" />
          {#if showOverlay && tab === "still"}
            <div class="overlay" style={regionStyle(finding.region, terminal)}></div>
          {/if}
        {/if}
      </div>
    </div>
  {/if}

  {#if finding.detail}
    <p class="detail">{finding.detail}</p>
  {/if}

  {#if onVerdict}
    <div class="review">
      {#if reviewOpen}
        <input
          class="note"
          placeholder="why? (this text trains the next calibration)"
          bind:value={note}
        />
        <div class="buttons">
          {#each VERDICTS as value}
            <button disabled={saving} onclick={() => record(value)}>{value}</button>
          {/each}
        </div>
      {:else}
        <button class="review-toggle" onclick={() => (reviewOpen = true)}>
          {verdict ? "change verdict" : "review"}
        </button>
      {/if}
    </div>
  {/if}

  <button class="why" onclick={() => (showWhy = !showWhy)}>
    {showWhy ? "hide" : "show"} how this was judged
  </button>
  {#if showWhy}
    <div class="why-body">
      <div><b>check</b> {finding.check ?? "model"}</div>
      <div><b>class</b> {finding.class}</div>
      {#if finding.measured !== undefined}<div><b>measured</b> {finding.measured.toFixed(3)}</div>{/if}
      <div><b>confidence</b> {finding.confidence.toFixed(2)}</div>
      {#if modelAnswers(finding).length}
        <table>
          <tbody>
            {#each modelAnswers(finding) as answer}
              <tr>
                <td>{answer.key}</td>
                <td>
                  <span class="meter"><span style={`width:${answer.value * 100}%`}></span></span>
                  {answer.value.toFixed(2)}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
    </div>
  {/if}
</article>

<style>
  .card {
    background: #14161d;
    border: 1px solid #232734;
    border-radius: 10px;
    padding: 14px 16px;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .card.below { opacity: 0.72; }
  header { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
  h3 { margin: 0; font-size: 15px; font-weight: 600; letter-spacing: 0.01em; }
  .chip {
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: 0.06em;
    padding: 3px 8px;
    border-radius: 999px;
    border: 1px solid transparent;
    font-weight: 700;
  }
  .prob, .verified, .events {
    font-size: 11px;
    color: #9aa4b8;
    border: 1px solid #262b38;
    border-radius: 999px;
    padding: 2px 8px;
  }
  .brief { margin: 0; color: #c9d1e0; line-height: 1.45; }
  .meta { display: flex; gap: 14px; flex-wrap: wrap; font-size: 11.5px; color: #8b93a7; }
  .meta b { color: #cdd5e4; font-weight: 600; }
  .tags { color: #6f7789; }
  .evidence { display: flex; flex-direction: column; gap: 6px; }
  .tabs { display: flex; gap: 6px; }
  .tabs button {
    background: #1b1e28;
    color: #9aa4b8;
    border: 1px solid #262b38;
    border-radius: 6px;
    font-size: 11px;
    padding: 3px 10px;
    cursor: pointer;
  }
  .tabs button.active { color: #e8edf7; border-color: #3a4260; background: #232838; }
  .viewport { position: relative; background: #0b0c10; border-radius: 8px; overflow: hidden; }
  .viewport img { display: block; width: 100%; image-rendering: pixelated; }
  .viewport video { display: block; width: 100%; }
  .overlay {
    position: absolute;
    border: 2px solid #ffd60a;
    box-shadow: 0 0 0 9999px rgba(0, 0, 0, 0.18);
    pointer-events: none;
  }
  .detail { margin: 0; font-size: 12px; color: #8b93a7; }
  .review { display: flex; flex-direction: column; gap: 6px; }
  .review-toggle {
    align-self: flex-start;
    background: #1b1e28;
    border: 1px solid #262b38;
    color: #9aa4b8;
    border-radius: 6px;
    font-size: 11px;
    padding: 3px 10px;
    cursor: pointer;
  }
  .review .buttons { display: flex; gap: 6px; flex-wrap: wrap; }
  .review .buttons button {
    background: #161a23;
    border: 1px solid #2a3040;
    color: #cdd5e4;
    border-radius: 6px;
    font-size: 11px;
    padding: 3px 9px;
    cursor: pointer;
  }
  .review .buttons button:hover { background: #232838; }
  .note {
    background: #101319;
    border: 1px solid #262b38;
    border-radius: 6px;
    color: #dfe5f0;
    font-size: 11.5px;
    padding: 5px 8px;
  }
  .verdict {
    font-size: 10.5px;
    border-radius: 999px;
    padding: 2px 8px;
    color: #ffb4b4;
    border: 1px solid #4d1f1f;
    background: #2a1414;
  }
  .verdict.good { color: #8ce99a; border-color: #1f4d33; background: #10251a; }
  .why {
    align-self: flex-start;
    background: none;
    border: none;
    color: #6c9aff;
    font-size: 11.5px;
    cursor: pointer;
    padding: 0;
  }
  .why-body { font-size: 11.5px; color: #9aa4b8; display: flex; flex-direction: column; gap: 4px; }
  .why-body b { color: #cdd5e4; font-weight: 600; margin-right: 6px; }
  table { border-collapse: collapse; }
  td { padding: 2px 10px 2px 0; }
  .meter {
    display: inline-block;
    width: 80px;
    height: 6px;
    background: #232734;
    border-radius: 3px;
    overflow: hidden;
    margin-right: 6px;
    vertical-align: middle;
  }
  .meter span { display: block; height: 100%; background: #6c9aff; }
</style>
