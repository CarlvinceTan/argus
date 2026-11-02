# Running Argus

Everything here lives in `~/code/proj/argus` and is self-contained: Go module,
bundled test subjects, benchmark data.

## 1. The tool

```sh
go test ./... && go vet ./...
go build -o var/argus ./cmd/argus
go build -o var/argus-bench ./cmd/argus-bench

# calibrated tier: TypeSafe Jev through Command Code (free while the deal lasts)
./var/argus run --provider jev --subject midas \
  --cmd "$HOME/.local/bin/midas" --steps 6 --out var/runs/midas

# open-source calibrated tier: llama.cpp token probabilities, no hosted calls
ARGUS_PROVIDER=opensource ARGUS_LOCAL_BASE_URL=http://127.0.0.1:8081/v1 \
ARGUS_LOCAL_MODEL=local ./var/argus run --provider opensource --offline \
  --subject auth --cmd "python3 testdata/audit-subjects.py --mode=login" --steps 4

# LLM tier through Midas's pkg/ai (Command Code gateway by default)
./var/argus run --provider midasai --subject midas \
  --cmd "$HOME/.local/bin/midas" --steps 3 --out var/runs/midasai

# hosted chat tier (commandcode gateway, deepseek-v4.1-flash + vision)
./var/argus run --provider commandcode --subject midas \
  --cmd "$HOME/.local/bin/midas" --steps 6 --out var/runs/midas

# self-hosted, offline: requires a loopback OpenAI-compatible server
# (llama.cpp on this machine, or an ollama tunnel: ssh -N -L 11434:127.0.0.1:11434 aorus)
ARGUS_LOCAL_BASE_URL=http://127.0.0.1:11434/v1 ARGUS_LOCAL_MODEL=qwen3:14b \
./var/argus run --provider local --sandbox-self --vision=false \
  --subject midas --cmd "$HOME/.local/bin/midas" \
  --env=MIDAS_PROVIDER=openai --env=MIDAS_MODEL=qwen3:14b \
  --env=MIDAS_BASE_URL=http://127.0.0.1:11434/v1 --env=MIDAS_API_KEY=local \
  --steps 6 --out var/runs/midas-offline

# TypeSafe Jev through OpenRouter (needs credits: OPENROUTER_API_KEY or the
# key already stored by opencode)
./var/argus run --provider openrouter --subject demo \
  --cmd "python3 testdata/defective-tui.py" --steps 4

# TypeSafe Jev on OpenCode's Console API (OPENCODE_API_KEY, or the key that
# `opencode auth login` saved); ARGUS_JEV_MODEL=jev-1.13-free while it lasts
./var/argus run --provider opencode --subject demo \
  --cmd "python3 testdata/defective-tui.py" --steps 4
```

Every decision provider takes the same typed questions (`noul`, `choice`, `score`) about any state, so anything that asks questions (the audit, the change filter) works on all of them. `internal/provider/conformance_test.go` holds each one to that: System One with Jev's native labels (`opencode`, `openrouter`) or single-token labels (`jev`, `systemone`), chat models emulating decisions (`commandcode`, `local`) and llama.cpp logprobs (`opensource`). Each must answer all three types, pass the whole state to its model, and run the change filter. A System One endpoint that wants the other label style takes `ARGUS_JEV_LABELS=native|tokens`.

`--sandbox-self` re-executes the whole pipeline inside a loopback-only sandbox
(`internal/isolate`), so during an offline evaluation neither the analyser nor the
subject can reach the internet. The run records `isolation.self_canary` with the
probe results as evidence. `argus canary` runs the same probes standalone.

## 1b. The GUI path

```sh
# on the GUI host (aorus): virtual display + agent + subject
setsid Xvfb :99 -screen 0 1400x900x24 -ac -listen tcp &
setsid ~/argus-x11-agent-linux -port 7011 -display :99 -window TaskBoard &
setsid env DISPLAY=:99 ~/gui-demo-linux &

# on the analyser host: forward the agent port, then run
ssh -N -L 7011:127.0.0.1:7011 aorus &
./var/argus x11probe --x11-agent localhost:7011 --seconds 3 --inputs --out var/x11.png
./var/argus run --mode gui --x11-agent localhost:7011 --subject gui-demo \
  --cmd "ssh -o BatchMode=yes aorus env DISPLAY=:99 ./gui-demo-linux" \
  --steps 4 --provider commandcode --out var/runs/gui
```

Build the Linux side with `GOOS=linux GOARCH=amd64 go build -o var/gui-demo-linux ./testdata/gui-demo`
and the same for `./cmd/argus-x11-agent`.

## 2. Review loop

```sh
./var/argus triage label --run var/runs/x --finding "clipped text" \
  --verdict accepted --note "header truncates the cwd path, no ellipsis"
./var/argus triage label --run var/runs/x --finding "broken border" \
  --verdict rejected --note "midas draws panels with open sides by design"
./var/argus triage stats
./var/argus triage calibrate      # writes var/triage/calibration.json
./var/argus triage reset --yes    # back to stock behaviour
```

## 2b. Web console (CLI + web)

```sh
go build -o var/argus-web ./cmd/argus-web
./var/argus-web --addr 127.0.0.1:8787                  # local
./var/argus-web --addr 0.0.0.0:8787 --token "$(openssl rand -hex 16)"   # network
```

The console is a served Svelte app (no desktop bundle), so it works the same when
the auditor runs on a remote or headless host: it lists runs, shows the backlog
with evidence and highlights, records review verdicts, rebuilds the calibration,
and can start a new audit and stream its log. Mutating endpoints require the token
whenever the console is bound off-loopback, and it refuses to start without one in
that case.

The frontend is built with `cd web && npm run build`, which writes into
`internal/webui/dist` and is embedded into the binary; `npm run dev` proxies the
API to `127.0.0.1:8787` for UI work.

## 3. Benchmark

```sh
./var/argus-bench run   --argus ./var/argus --provider commandcode
./var/argus-bench score --out benchmark/results/scorecard.json
```

Subjects (see `benchmark/subjects.json`): fzf 0.65.1 vs 0.65.2 (documented
inline-right truncation fix, #4479), btop 1.4.5 vs 1.4.6 (proc-list redraw,
border overflow, box alignment), and a synthetic TUI with six known injected
defects. Ground truth never reaches the model.

## 4. Visualiser (Wails v3 + Svelte)

```sh
cd visualizer/argusview
wails3 build            # produces bin/argusview
./bin/argusview         # run it from the argus root so it finds var/runs
```

Backlog sorted by severity, grouping by page/feature, annotated stills with a
region overlay, frame strips and video clips, candidate traces, review buttons
with a note field, and a learning panel showing per-check precision.

## 5. Polymux testing

Polymux test sessions use the OpenCode Go subscription as the model, and the UniMelb student Grok Bot (the student account, separate browser profile) as the reference account. After every session, the test conversations, agents and artifacts the session created on that Grok Bot must be cleaned up, and only those. See [docs/polymux-testing.md](docs/polymux-testing.md).

Open items: confirm the OpenCode Go OpenAI-compatible base URL and key supply before adding a native provider.

## 6. Polymux UI lane

```sh
go build -o var/argus ./cmd/argus

# what a change can affect, three journeys at a time
./var/argus polymux ui --repo ~/code/proj/polymux --out var/ui \
  --changed apps/Polymux/Sources/Views/ChatPane.swift --parallel 3 --replay var/replay

# the whole surface, one CI shard of four
./var/argus polymux ui --repo ~/code/proj/polymux --out var/ui --shard 2/4

# what a branch can affect: area rules and baseline links, then a decision model drops the rest
./var/argus polymux ui --repo ~/code/proj/polymux --out var/ui \
  --changed-since origin/main --filter jev --dry-run

# fail only on issues that aren't already open; main or nightly runs add --record
./var/argus polymux ui --repo ~/code/proj/polymux --out var/ui --issues var/issues.json
```

It prints `ARGUS: PASS|FAIL n findings, t s` and writes `swarm.md`, `swarm.json` and a run directory per journey (`run.json`, stills, `trace.json`, `states.mmd`, `app.log`). Only issues rated noticeable or worse fail a run.

It builds the app once and every journey uses that build, so a tree that moves mid-run can't mix versions. It stops first if the app doesn't build or the screen is locked.

What it checks on each state:

| Area | Checks |
| --- | --- |
| Layout | Controls overlapping, controls outside the window, short labels cut off, blank windows (confirmed from pixels) |
| Behaviour | Controls that do nothing after two presses, crashes, runtime warnings in `app.log` |
| Side effects | A press moving controls outside its pane; text or a spinner that flashes past while the window settles |
| Accessibility | Unnamed controls, small targets (a hint, because an app can widen its hit area) |
| Performance | Response time per action, longest main-thread hold, memory growth, idle CPU |

Field actions use `internal/agent/sample_data.go`: plausible worst cases such as long names, unbreakable addresses, right-to-left text and pasted lines, not random strings.

Performance numbers are reported with the machine's load and never fail a run. A single sample on a shared machine isn't a regression; that needs a baseline (see PLAN.md).

Measurements that are unreliable alone get a second look before they count: tree findings are re-read after 600 ms, and an inert control is pressed twice. Flash detection reads the tree, which SwiftUI updates late, so it caught a 350 ms flash and missed a 200 ms one. Faster flashes need frame recording.

`benchmark/polymux/patches/ui-seeds.patch` plants one bug per check. A new check isn't trusted until it fails on a planted bug.

- **Journeys** come from Polymux's `e2e/areas.json` and the `// @fixture` line of each flow, so no model plans them. Flows that share a state are one journey. Real-server flows and states that aren't plain fixture runs are not covered.
- **Isolation:** each journey starts its own `scripts/run.sh --fixture` instance from the shared build and stops it afterwards, which deletes its data. Input goes through Polymux's Control helper, so the app never takes focus. Resizing is the one call that goes to `cua-driver`.
- **`--provider none`** is the default: accessibility measurements only, no model calls. Pixel motion stays in `run.json` without a model, because it can't tell flicker from a typing indicator.
- **`--replay DIR`** reuses the action trace of the last run that didn't crash. Replayed steps need no model to pick an action, and a screen identical to the trace needs no model to judge it. If a step's control is gone, the journey explores from there. Every run also writes its own `trace.json`.
- **Captures run one at a time.** Simultaneous ScreenCaptureKit requests fail, so journeys share a gate and retry. A step with no screenshot still runs its tree checks and is counted in `swarm.md`; a journey with no screenshot at all counts as a failed run, never as clean.
- **`--control DIR`** points at a checkout with the granted helper, for testing another copy of Polymux.
- **Baseline links:** `e2e/areas.json` is written by hand and forgets things. A changed Swift file that contains a label a journey's approved baseline shows (several words, shown by at most 3 journeys) adds that journey, and the dry run says why. On Polymux this closed 8 missed links across 4 files: a `ShellView.swift` change used to skip `fixture-new-bot` and `fixture-new-group`, which open from it. Links only add.
- **Change filter:** `--changed-since REF` plans from the branch's diff against its merge base with REF, working tree included (CI needs `fetch-depth: 0`). `--filter PROVIDER` then asks a decision model "can this change affect it?" once per journey, and leaves out journeys it rates under 20%. It errs toward running: a lockfile, fixture, `e2e/intents.json` or `scripts/run*` change runs everything the rules picked, and so do a diff over 60 kB and a failed call. A journey whose own flow or baseline changed is never asked, and an unanswered one runs. The model sees each journey's flow comments, the controls its flows use and its baseline's labels, 20 journeys per call (about 9k tokens on Polymux). Secret-looking files (`.env*`, keys) are named but their content isn't sent. `swarm.md` lists every left-out journey and why; `--dry-run` prints the plan without building. The design follows tester-army/e2e's `decisionFilter`. Measured 2 Nov 2026 with `--filter opencode` and `jev-1.13-free`: it kept every journey whose screens showed a planted change (8 of 8, rated 0.67 to 0.90) and answered in 1 to 3 s for 56 journeys. The savings were modest: a label rename in an unmapped file went from 56 journeys to 34, a nearby layout edit kept all 56, and on real commits it left nothing out once the server rule below had run. Keep it optional.
- **Server code:** journeys run on fixtures, and `scripts/run.sh` neither builds nor starts the Go server for a fixture run, so changes confined to the server's sources plan no journeys. The sources come from run.sh's own `fingerprint ... server.stamp` line. Of Polymux's last 11 commits, 6 made the area rules run all 56 journeys; 5 of those touched only server code and now run none.
- **Issue ledger:** `--issues FILE` compares the run with every issue recorded before. Each issue in `swarm.md` reads new, known (since when, how many runs) or reopened, and issues the run looked for and no longer saw are listed as gone. Issues the ledger holds open don't fail the run; new and reopened ones do. Only `--record` changes the ledger, and only runs that define "known" (main, nightly) should use it, because a ledger that learned from every run would make a regression known on the next rerun. An issue is gone only where the run looked: its journey ran with every screenshot. Numbers in an element read as `#`. Replayed over 13 earlier runs, `unnamed Button` was in 12 and failed all of them; with the ledger, 6 of 13 failed, each on a new issue.
- **Intents** are covered in section 7.


## 7. Intent ledger

`e2e/intents.json` in the app's repository records behaviour that looks like a defect but is meant. It changes in the same commit as the code it describes.

```sh
./var/argus intent add --repo ~/code/proj/polymux --id warning-dismiss-hit-area \
  --check structure_ax_small_target --element 'Button "Dismiss warning"' \
  --reason 'tapArea(14) widens the hit area past the 8pt frame' \
  --paths apps/Polymux/Sources/Views/WarningBubble.swift
./var/argus intent list --repo ~/code/proj/polymux     # exits 1 if any intent is stale
./var/argus intent confirm --repo ~/code/proj/polymux --id warning-dismiss-hit-area
./var/argus intent remove --repo ~/code/proj/polymux --id warning-dismiss-hit-area
```

- **Matching:** an intent covers one check and an element pattern, optionally one journey. A match stays in `run.json` but leaves the backlog.
- **Staleness:** each intent stores a digest of the source files behind it. If they change or vanish, the intent suppresses nothing and the finding returns with the commands to confirm or remove it. So an old "this is on purpose" can't hide a new regression.
- **Models:** confirmed intents join the policy block a model reads. Stale ones don't.
- **Pruning:** on a whole-surface run, `swarm.md` lists intents nothing matched.
- **Reporting:** every issue in `swarm.md` carries the `argus intent add` command for it.

`argus triage` stays for tuning a whole check on this machine. Use an intent when one element is meant to behave this way, because it travels with the code.

## 8. Drift baselines

A defect check can't flag a redesign nobody asked for, because the new UI is well formed. Baselines catch it: each journey has an approved state for every step, and `argus polymux ui` compares against it, reporting what appeared, vanished, moved or looks different. That fails the run.

```sh
./var/argus baseline list --repo ~/code/proj/polymux
./var/argus polymux ui --repo ~/code/proj/polymux --out var/ui --no-baseline     # explore and record
./var/argus baseline approve --repo ~/code/proj/polymux --run var/ui \
  --journey fixture-new-bot --reason 'New Bot redesign, approved by Carlvince'
```

- **Read-only:** only `argus baseline approve` writes a baseline, and it needs a reason. It refuses a journey whose run crashed, failed or lost a screenshot, and prints how much the approval changes. `--confirm DIR` names a second run and approves only what both reached, because one recording can catch a moment the app doesn't reliably reach; `--all` requires it. Replay traces update themselves, so they guard nothing; baselines outrank them.
- **Where it lives:** `e2e/baselines/<journey>.json` in the app's repository, one line per element, so a changed screen reads as a plain diff in review. Screenshots stay in `--images DIR` (default `var/baselines`), because they are large and specific to the machine. Without them, structure is still compared.
- **Exact steps:** a baselined journey takes the baseline's steps and nothing else. `--no-baseline` explores instead. A control that has gone stops the journey and is reported; an action that fails for another reason makes the run inconclusive, not drifted.
- **Retries:** a journey that drifted or couldn't finish runs once more, alone. A real change fails again; a one-off clears, and `swarm.md` lists what the first attempt saw. On unchanged code a full run needed this for about 3 of 47 journeys, and all three cleared.
- **Timing:** states are saved at rest, and a compared screen gets 4 seconds to reach its baseline. Only a difference that stays counts, so a slow fetch or a splash isn't drift.
- **What's ignored:** layout containers, status probes and numbers, which read as `#` so that clocks and instance numbers don't drift. Vertical position inside a scroll area isn't compared, because it depends on the scroll; size and horizontal position are.
- **What can't be baselined:** `fixture-profile-sync` tests a live rename whose timing differs between runs, so `--confirm` refuses it. It explores instead of comparing.
- **Real variation:** record it as an intent, whose check and element are patterns: New Bot's random avatar is `structure_drift_*` on `*avatar, Idle*`. Pixels under a known variation are masked too.
