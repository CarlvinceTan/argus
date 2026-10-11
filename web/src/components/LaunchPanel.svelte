<script lang="ts">
  // Start an audit from the browser. This is what makes the console usable on a
  // remote host: no shell needed to kick off a run or watch it.
  import { api, ApiError, setToken, token } from "../lib/api";

  interface Props {
    onFinished?: () => void;
  }

  interface Launched {
    id: string;
    subject: string;
    command: string;
    mode: string;
    provider: string;
    status: string;
    dir: string;
    startedAt: string;
    exitCode?: number | null;
    log: string[];
    error?: string;
  }

  interface Persona {
    id: string;
    label: string;
    hint: string;
  }

  let { onFinished }: Props = $props();
  let open = $state(false);
  let busy = $state(false);
  let error = $state("");
  let launched = $state<Launched[]>([]);
  let watching = $state(false);

  let subject = $state("midas");
  let command = $state("$HOME/.local/bin/midas");
  let mode = $state<"tui" | "gui">("tui");
  let provider = $state("commandcode");
  let steps = $state(6);
  let cols = $state(100);
  let rows = $state(30);
  let display = $state("localhost:99");
  let agentAddr = $state("localhost:7011");
  let windowMatch = $state("");
  let offline = $state(false);
  let vision = $state(true);
  let textInput = $state(false);
  let personas = $state<string[]>([]);
  let tokenInput = $state(token());

  // Personas mirror the CLI: each is a specialist reviewer with its own
  // question bank, merged into one backlog.
  const PERSONAS: Persona[] = [
    { id: "visual", label: "visual", hint: "rendering, alignment, overlap, contrast" },
    { id: "animation", label: "animation", hint: "flicker, churn, layout drift" },
    { id: "behaviour", label: "behaviour", hint: "flows, dead ends, missing affordances" },
    { id: "completeness", label: "completeness", hint: "features that should exist but do not" },
    { id: "errors", label: "error handling", hint: "failure paths and recovery" },
  ];

  async function refresh() {
    try {
      launched = (await api.launched()) as Launched[];
    } catch {
      launched = [];
    }
  }

  async function start() {
    if (busy) return;
    busy = true;
    error = "";
    if (tokenInput) setToken(tokenInput);
    try {
      await api.launch({
        subject,
        command,
        mode,
        provider,
        steps,
        cols,
        rows,
        display,
        agentAddr,
        window: windowMatch,
        offline,
        vision,
        textInput,
        allowClicks: true,
        personas,
      });
      watching = true;
      await refresh();
      startPolling();
    } catch (exc) {
      if (exc instanceof ApiError && exc.status === 401) {
        error = "the console rejected the token; paste the token it was started with";
      } else {
        error = String(exc);
      }
    } finally {
      busy = false;
    }
  }

  let timer: number | undefined;

  function startPolling() {
    if (timer) return;
    timer = window.setInterval(async () => {
      await refresh();
      const running = launched.some((run) => run.status === "running");
      if (!running) {
        window.clearInterval(timer);
        timer = undefined;
        onFinished?.();
      }
    }, 2000);
  }

  $effect(() => {
    if (open && !watching) {
      refresh();
    }
  });

  function togglePersona(id: string) {
    personas = personas.includes(id) ? personas.filter((value) => value !== id) : [...personas, id];
  }
</script>

<section class="launch">
  <button class="toggle" onclick={() => (open = !open)}>
    {open ? "close" : "new audit"}
  </button>
  {#if launched.some((run) => run.status === "running")}
    <span class="running">audit running…</span>
  {/if}

  {#if open}
    <div class="form">
      <label>subject <input bind:value={subject} /></label>
      <label class="wide">command <input bind:value={command} /></label>
      <label>mode
        <select bind:value={mode}>
          <option value="tui">tui</option>
          <option value="gui">gui (X11)</option>
        </select>
      </label>
      <label>provider
        <select bind:value={provider}>
          <option value="commandcode">commandcode (hosted API)</option>
          <option value="openrouter">openrouter (Jev, calibrated)</option>
          <option value="opensource">open source (llama.cpp logprobs)</option>
          <option value="local">local (chat model)</option>
        </select>
      </label>
      <label>steps <input type="number" bind:value={steps} min="1" max="60" /></label>
      {#if mode === "tui"}
        <label>cols <input type="number" bind:value={cols} /></label>
        <label>rows <input type="number" bind:value={rows} /></label>
      {:else}
        <label>display <input bind:value={display} /></label>
        <label>agent <input bind:value={agentAddr} /></label>
        <label>window <input bind:value={windowMatch} placeholder="WM_NAME substring" /></label>
      {/if}
      <label class="check"><input type="checkbox" bind:checked={vision} /> vision tier</label>
      <label class="check"><input type="checkbox" bind:checked={offline} /> offline (loopback only)</label>
      <label class="check"><input type="checkbox" bind:checked={textInput} /> allow typing</label>
      <label class="wide">token <input bind:value={tokenInput} placeholder="required when the console is served off-loopback" /></label>

      <div class="personas wide">
        <span>personas</span>
        {#each PERSONAS as persona}
          <button
            class:active={personas.includes(persona.id)}
            title={persona.hint}
            onclick={() => togglePersona(persona.id)}
          >{persona.label}</button>
        {/each}
        <em>none selected = the default set</em>
      </div>

      <button class="start" disabled={busy} onclick={start}>{busy ? "starting…" : "start audit"}</button>
      {#if error}<p class="error">{error}</p>{/if}
    </div>
  {/if}

  {#if launched.length > 0}
    <div class="launched">
      {#each launched as run}
        <details class:active={run.status === "running"}>
          <summary>
            <b>{run.subject}</b>
            <span class="status {run.status}">{run.status}</span>
            <span class="meta">{run.mode} · {run.provider} · {run.id}</span>
          </summary>
          <div class="log">
            {#if run.error}<p class="error">{run.error}</p>{/if}
            <pre>{run.log.slice(-120).join("\n")}</pre>
            {#if run.status === "running"}
              <button onclick={() => api.stopLaunch(run.id).then(refresh)}>stop</button>
            {/if}
          </div>
        </details>
      {/each}
    </div>
  {/if}
</section>

<style>
  .launch { margin: 14px 0 4px; }
  .toggle, .start {
    background: #1b1e28;
    border: 1px solid #2a3040;
    color: #cdd5e4;
    border-radius: 6px;
    font-size: 11.5px;
    padding: 4px 12px;
    cursor: pointer;
  }
  .running { margin-left: 10px; font-size: 11px; color: #ffd60a; }
  .form {
    margin-top: 10px;
    padding: 12px 14px;
    background: #101319;
    border: 1px solid #1c1f2a;
    border-radius: 10px;
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(190px, 1fr));
    gap: 10px 14px;
    align-items: end;
  }
  label { display: flex; flex-direction: column; gap: 4px; font-size: 11px; color: #8b93a7; }
  label.wide { grid-column: 1 / -1; }
  label.check { flex-direction: row; align-items: center; gap: 6px; }
  input, select {
    background: #161a23;
    border: 1px solid #232734;
    color: #dfe5f0;
    border-radius: 6px;
    padding: 5px 8px;
    font-size: 11.5px;
  }
  .personas { display: flex; gap: 6px; align-items: center; flex-wrap: wrap; font-size: 11px; color: #8b93a7; }
  .personas button {
    background: #161a23;
    border: 1px solid #232734;
    color: #9aa4b8;
    border-radius: 999px;
    font-size: 11px;
    padding: 3px 10px;
    cursor: pointer;
  }
  .personas button.active { color: #e8edf7; border-color: #3a4260; background: #232838; }
  .personas em { color: #5b6274; font-style: normal; }
  .start { grid-column: 1 / -1; justify-self: start; }
  .error { color: #ff8080; font-size: 11.5px; grid-column: 1 / -1; }
  .launched { margin-top: 10px; display: flex; flex-direction: column; gap: 6px; }
  details { background: #101319; border: 1px solid #1c1f2a; border-radius: 8px; padding: 6px 10px; }
  details.active { border-color: #3a4260; }
  summary { cursor: pointer; font-size: 11.5px; color: #9aa4b8; display: flex; gap: 10px; align-items: baseline; }
  summary b { color: #dfe5f0; }
  .status { color: #6f7789; }
  .status.running { color: #ffd60a; }
  .status.done { color: #8ce99a; }
  .status.failed { color: #ff8080; }
  .meta { color: #5b6274; }
  .log pre {
    max-height: 220px;
    overflow: auto;
    background: #0b0c10;
    border-radius: 6px;
    padding: 8px;
    font-size: 11px;
    color: #9aa4b8;
  }
</style>
