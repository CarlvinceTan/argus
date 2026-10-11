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
```

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
