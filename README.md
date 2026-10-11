# Argus

Argus is an autonomous UI defect hunter. It drives a terminal (or, by extension,
any GUI with a harness) in an isolated environment, watches what the interface
does frame by frame, and reports the problems it can defend: broken borders,
clipped values, duplicated rows, unreadable text, stuck spinners, flicker,
layout drift, and the "obvious even though nobody wrote it down" expectations
users bring to an interface.

It is built as a *pipeline*, not a prompt:

```
pty harness  ->  terminal world model  ->  deterministic checks   \
                                    \->  animation analysis        ->  decision tier  ->  triage  ->  backlog + evidence
vision tier (only when the decision tier is unsure)               /
```

The two reasoning tiers are deliberately different:

| Tier | What it is | Job |
| --- | --- | --- |
| Decision tier | TypeSafe **Jev** through OpenRouter's decisions router (`noul` / `choice` / `score` answers with probabilities), or any OpenAI-compatible chat model (self-hosted Qwen3, or a hosted gateway) | Answers many small, typed questions about one screen: is this a defect, which class, how bad, could it be intentional |
| Vision tier | An open-source or hosted multimodal model | Blinded description of cropped evidence. It is never asked "is this broken?"; code matches its description against the candidate's defect class, which keeps it from agreeing with a hypothesis |

Code owns the workflow. Models never invent candidates, never set thresholds,
and never produce shell commands, selectors or keystrokes. Every finding carries
the evidence that produced it: the screen region, the check or question that
flagged it, the measured value, and annotated stills/strips/video.

## What makes it different from a screenshot lint

* **Deterministic first.** Ten checks run on the cell grid (characters *and*
  attributes): border continuity and coverage, content drawn over borders,
  ragged panel edges, duplicated lines, clipped tokens, truncation markers with
  room to spare, invisible text, WCAG contrast, raw escape/format artifacts,
  progress-bar/label disagreement, column misalignment, placeholder junk.
* **Animation is a first-class signal.** The harness keeps a frame ledger (one
  entry per screen change with a changed-cell count and the input that preceded
  it). From that timeline Argus measures flicker (A→B→A cell oscillations),
  full-screen clear-and-repaint events, redraw churn while idle, footer/layout
  drift, irregular animation cadence, and spinners that never advance.
* **Expectations, not just pixels.** A POLICY block in every decision call
  carries the heuristics a person applies without being told: focused lists show
  a selection, footers advertise the keys that work here, dialogs show how to
  escape, empty states explain themselves, loading states animate. That is what
  produces the "unstated expectation" findings.
* **Noise is a design constraint, not an afterthought.** Candidates are
  falsifiable predicates with locations; the decision tier judges each one;
  uncertain findings must survive a *blinded* vision check or stay out of the
  backlog; a specific finding supersedes a screen-wide one of the same class;
  each page contributes at most five findings (the rest stay in `run.json`).
* **Everything is auditable.** `run.json` contains observations, candidate
  traces with the model's verdict on each, rejected findings, the provider
  manifest, model/token/cost accounting, and the isolation evidence.

## The auditor: feature inventory, gaps and behaviour

Rendering checks answer "is this screen drawn correctly". They cannot answer the
question a software audit actually asks: *does this software do what it should,
and is anything obviously missing?* That is a separate package, `internal/audit`,
and it works in three measured steps before any model is consulted:

1. **Inventory.** Every captured screen is classified deterministically: which
   capabilities it shows (a login form, a list, a detail view, a destructive
   action, a search field, an empty state), which controls it advertises, what its
   lists look like, and how many screens the run visited. Ambiguity is resolved by
   run shape, not by one row at a time: three aligned rows are a table, three
   colon-labelled rows are one record's fields, and a hint bar is neither.
2. **Expectations.** A small, auditable table states which capabilities imply
   which companions: a sign-in screen implies account recovery and a visible
   failure path; a list implies a way to open an item, an empty state and (at
   scale) search; a destructive action implies confirmation; a detail view implies
   a way back. Gaps are generated from the table, each with the evidence that
   triggered it.
3. **Structure.** Nesting depth (measured from `┌…┐`/`└…┘` rectangles, with a
   panel-pairing fallback for clipped screens), competing focus regions, repeated
   values, dead-end screens and advertised-but-inert controls are measured on the
   screen where they appear.

Only then does the decision tier speak, and only about meaning: is the measured
absence a real problem *for this application*, what class is it, how bad is it.
**Measured facts do not need a model's permission to exist**: a feature gap or a
structural measurement is reported on its measurement, blended with the model's
judgement, and the model can still call it a non-issue. Visual findings keep the
opposite rule, because there the model is deciding whether a measurement is a
defect at all. That split is the answer to "should this be on the LLM side or the
Jev side": code measures, the calibrated tier judges.

## Reviewers (personas)

One reviewer answering thirty questions answers all of them shallowly, so a run
deploys several narrow ones over the same evidence and merges the result:

| Persona | Looks at | Default |
| --- | --- | --- |
| `visual` | rendering, alignment, overlap, contrast, encoding artefacts | yes |
| `behaviour` | flows, dead ends, inert controls, repetition, animation over time | yes |
| `completeness` | the feature inventory: what the application does not offer | yes |
| `animation` | movement only: flicker, cadence, idle repaint, drift | no |
| `errors` | failure paths, visible and explained errors, recovery | no |

```sh
argus run --subject midas --cmd midas --persona visual --persona completeness
```

## GUI subjects

The same pipeline runs against a real GUI, with the sensors swapped:

```
X11 window (Xvfb) ──▶ frame ledger ──▶ pixel sensors (churn, flicker, repaint, drift)
                                            │
                                            ├──▶ vision tier: blinded description + labelled tile grid
                                            └──▶ decision tier: POLICY + measured candidates ──▶ triage
```

* `internal/harness/x11.go` drives an X server directly (xgb): it finds the
  subject's window, injects real input with XTEST, resizes the window and reads
  pixels back. It skips *unmapped* windows and prefers one whose pixels actually
  change, because a long-lived X server accumulates dead windows from earlier
  runs that look exactly like the live one.
* `internal/sensors/pixels.go` measures what pixels can prove: flicker per tile
  (alternation over the whole frame sequence, so it does not depend on cadence),
  full-window repaints, content drift and idle repaint churn. Everything that
  pixels cannot decide (a clipped word, overlapping text, low contrast) is judged
  by the vision tier, which is asked to *describe* and to name the labelled tile
  (A1, B2, …). Code maps that label back to pixel coordinates, so a finding always
  has a region a reviewer can see highlighted.
* `argus-x11-agent` runs on the machine that hosts the GUI. Measured reality: an
  900x560 window is 2 MB per frame and a tunnel moves ~1.4 MB/s, so a remote
  harness cannot sample a 10 Hz animation without aliasing. The agent captures and
  runs the *same* sensor code locally (imported from `internal/sensors`, not
  reimplemented), then sends change metrics plus the PNGs of frames that changed.
* X11 has a hard 256 KB request limit; a full-window `PutImage` is rejected with
  `BadLength`, so uploads are done in strips. Both traps are covered by the demo
  subject in `testdata/gui-demo`, which also carries ten deliberately injected
  defects (clipped label, duplicated row, low-contrast text, overlapping text,
  blinking dot, idle tick, wrong progress label, an empty panel, a transparent
  dropdown, a disabled-looking button).

```sh
# on the GUI host
Xvfb :99 -screen 0 1400x900x24 -ac -listen tcp &
./argus-x11-agent-linux -port 7011 -display :99 -window TaskBoard &

# on the analyser host (tunnel the agent's loopback port)
ssh -N -L 7011:127.0.0.1:7011 <gui-host> &
./var/argus run --mode gui --x11-agent localhost:7011 --subject gui-demo \
  --cmd "ssh <gui-host> env DISPLAY=:99 ./gui-demo-linux" \
  --steps 4 --provider commandcode --out var/runs/gui
```

## Providers

The LLM tier runs through **Midas's own model layer** (`pkg/ai`): provider
adapters, streaming and error handling come from `github.com/CarlvinceTan/Midas`,
so Argus speaks to models the same way the rest of the toolchain does. Two
adapters are built on it — `MidasVision` (multimodal, the vision/semantic tier)
and `MidasDecisions` (the same chat model answering the typed questions) — and
the decision contract itself is shared with the plain-HTTP adapter: only the
transport differs.

```
--provider systemone    Jev-compatible System One endpoint (default: free,
                        key-less DiffusionGemma; Nouls use single-token true/false labels)
--provider jev          TypeSafe Jev via Command Code's System One endpoint
                        (model typesafe/jev, free through 2026-09-24, 500 calls/day)
--provider openrouter   TypeSafe Jev via OpenRouter (calibrated; needs credits)
--provider commandcode  a hosted chat model on the Command Code gateway
--provider opensource   llama.cpp token log-probabilities: an open-source,
                        self-hosted calibrated decision model
--provider midasai      decisions *and* vision through Midas's pkg/ai layer
                        (Command Code gateway by default; local endpoints too)
--provider local        a local chat model as the decision tier (offline)
```

Measured on the same gateway, the midas-ai tier is slower per decision than the
calibrated Jev endpoint because a reasoning model streams its thinking before the
answer (tens of seconds per batch versus about 0.7s for a System One call), which
is why the calibrated tier is the default for hosted runs and the midas-ai tier is
the integration path — and the required one when a run must go through Midas's
model configuration.

The vision tier is configured independently (`ARGUS_VISION_BASE_URL`,
`ARGUS_VISION_MODEL`, `ARGUS_VISION_API_KEY`). Reference setups:

| Role | Hosted | Open source / self-hosted |
| --- | --- | --- |
| Decision | Jev via OpenRouter, Command Code System One, or free DiffusionGemma System One | `--provider opensource`: llama.cpp `/completion` log-probabilities (single-token yes/no, choice, score); Qwen3 is a tested model |
| Screenshot decision | DiffusionGemma `typesafe/jev` vision tower accepts up to 8 data-URL images, so it can answer calibrated questions directly about a screenshot | local VLM via llama.cpp with a projector |
| LLM/vision agent | Command Code `deepseek-v4.1-flash` / `deepseek-v4-flash-vision-exp` | Midas `pkg/ai` adapters + streaming (`--provider midasai`) |

The free DiffusionGemma Space is a **guide page** for a separate public HF
Inference Endpoint (`/v1/systemone`, model `jev-latest`), not a Space runtime API.
Argus sends screenshot data URLs through System One's `images` field; the model's
vision tower reads them before answering the same typed questions. System One
validates noul answer labels as single tokens, so Argus normalises them to
`true`/`false` and splits larger question sets to isolate any invalid question.

A chat model is not a calibrated decision model, and Argus does not pretend
otherwise: the chat adapter asks for point answers, rebuilds distributions in
code with an assumed confidence, and marks the run `local`/`degraded` in the
manifest so those runs are never quoted as Jev runs.

## Isolation and fairness

Two sandbox profiles are written per run:

* `subject-offline.sb` — the program under test runs with **all network access
  denied** (loopback stays open so a subject can talk to a local model). This is
  what stops a benchmark subject from fetching its own bug report.
* `eval-loopback.sb` — the loopback-only profile for offline evaluation runs.

The profiles are only text, so `argus canary` proves what they do by running
real probes inside them:

```
$ argus canary --model-url http://127.0.0.1:11434/v1
internet_by_hostname  expected=fail     exit=6  passed=true
internet_by_ip        expected=fail     exit=7  passed=true
dns_resolution        expected=fail     exit=6  passed=true
loopback_model        expected=success  exit=0  passed=true
```

Benchmark runs record that evidence in `run.json` under `isolation`. The ground
truth for a benchmark never reaches the model: a run gets a command, a terminal
size and a step budget, nothing else.

## Review loop: findings become training data

Every finding can be labelled by a reviewer, with a free-text reason:

```sh
argus triage label --run var/runs/demo --finding "progress bar" \
  --verdict accepted --note "bar shows 40% while the label reads 90%"
argus triage label --run var/runs/demo --finding "unstated expectation" \
  --verdict rejected --note "generic expectation text, no concrete evidence here"
argus triage stats          # what has been judged so far
argus triage calibrate      # derive data the pipeline reads on the next run
argus triage reset --yes    # throw the learning away and go back to stock
```

* The log is append-only (`var/triage/labels.jsonl`): every verdict keeps the
  finding's features (check, class, group, severity, model probabilities,
  verification status) plus the note, so a relabelling is a new line and the
  history stays auditable.
* `calibrate` derives `var/triage/calibration.json`: per-check precision,
  a suppression list, a promotion list, and a probability remap built from
  labelled bands. **The pipeline reads that file, not a prompt.**
* The adjustment is a *blend*, never a veto, so history can lower a noisy
  pattern's priority but cannot make measured evidence disappear; findings that
  fall below the line stay in `run.json` as below-threshold.
* A misclassification counts as a real issue (it is one, with the wrong class),
  so labelling a class wrong never teaches the system to stay quiet.
* Scoping is deliberately simple today (per check, globally). Because the raw
  labels are kept, a richer model -- per check and subject, or a fitted
  threshold -- can be computed later from the same history without relabelling.

The visualiser exposes the same loop: each finding has review buttons and a note
field, and a *learning* panel shows the verdict counts, per-check precision, and
what is currently suppressed or promoted.

## Layout

```
cmd/argus          the tool
cmd/argus-bench    benchmark runner and scorecard
internal/harness   pty control, frame ledger, key encoding
internal/screen    grid model, structure derivation, PNG rendering, annotations
internal/sensors   deterministic checks + animation analysis
internal/knowledge the POLICY block, question bank, severity legend
internal/agent     explorer (indexed action space) and auditor (triage)
internal/report    run.json, report.md, stills, strips, clips
internal/isolate   sandbox profiles and canary probes
internal/triage    reviewer labels, calibration, learning data
benchmark          subjects, ground truth, results
```

See `RUN.md` for the exact commands used in development.

## Quick start

```sh
go test ./... && go vet ./...

# See it work on the bundled defective demo (no sandbox, hosted provider):
go run ./cmd/argus run \
  --subject demo --cmd "python3 testdata/defective-tui.py" \
  --provider commandcode --steps 4 --out var/runs/demo

# Offline, loopback model only:
ARGUS_LOCAL_BASE_URL=http://127.0.0.1:11434/v1 ARGUS_LOCAL_MODEL=qwen3:14b \
go run ./cmd/argus run --provider local --offline --vision=false \
  --subject demo --cmd "python3 testdata/defective-tui.py" --steps 4
```

## Benchmark

`benchmark/subjects.json` pairs real releases where a documented UI fix landed
(pre-fix vs post-fix) and a synthetic subject with known injected defects. The
scorer requires the defect *class and* a curated keyword, so a vague
right-category finding cannot inflate the count.

```sh
go run ./cmd/argus-bench run   --argus ./var/argus --provider local
go run ./cmd/argus-bench score --out benchmark/results/scorecard.json
```

## Results so far

| Subject | Tier | Detected (pre-fix) | Noise (post-fix) |
| --- | --- | --- | --- |
| fzf 0.65.1 vs 0.65.2 (info-line truncation, `--info=inline-right`, #4479) | Jev (hosted, calibrated) | 1/1, class+keyword | 0 findings |
| fzf 0.65.1 vs 0.65.2, same pair | **Qwen3:14b, self-hosted, offline** | 1/1 (`truncation_with_room` at 64x14, 100x30 and 120x40) | 0 truncation findings; only an idle-repaint observation |
| btop 1.4.5 vs 1.4.6 (proc-list redraw, border overflow, box alignment) | Jev | 2/2 class-level | 0 findings |
| btop 1.4.5 vs 1.4.6, same pair | Qwen3:14b, offline | 0/2 | 0 findings (a 14B chat model is not enough for a dense TUI this size) |
| synthetic demo TUI (six injected defects) | Jev | 5/6 | n/a |
| synthetic GUI (`testdata/gui-demo`, ten injected defects) | commandcode vision + chat | the blinking-dot flicker localised to its 32x32 px tile, plus vision findings for the sidebar | n/a |

The fzf differential is the sharpest evidence in the project: the pre-fix build
draws `probe-info-li··` where the fixed build draws the full `probe-info-line`.
The system flags the first with `truncation_with_room` (marker plus 45 empty
columns in front of the value) and says nothing about the second. The rule had to
be refined twice: first it fired on any long token touching the right edge (which
is normal right-alignment), then it stopped trusting truncation markers unless
there was room in front of them. Both mistakes are now covered by unit tests taken
from the real screens.

### Auditor battery

Six subjects that differ only in the feature under test, run offline with the
open-source calibrated tier (`--provider opensource`, no hosted calls):

| Subject | Expected | Result |
| --- | --- | --- |
| sign-in with no account recovery | password-reset gap | `auth exists but password_reset does not` (major, p=0.70) |
| sign-in that advertises recovery (control) | no such gap | no password-reset gap reported |
| connections list with nothing to open | detail-view gap | `list exists but detail_view does not` (major) + `Connections offers no way forward` |
| connections list with open/edit/delete (control) | no detail gap | no detail gap; instead `delete exists but confirmation does not` (major) |
| agents > model > thinking boxes | nesting finding | `structure nesting` p=0.81: "content is nested 4 boxes deep: Agents > Agent: coder > Model: gpt-x > Thinking" |
| the same information, no nesting (control) | no nesting finding | nothing reported about structure |

The controls matter as much as the positives: the system is silent about the
feature that exists, and only reports the gaps that are measured. The same
findings were produced with TypeSafe Jev as the decision tier while the free
quota (500 calls/day through Command Code's System One endpoint) lasted; the
open-source tier is unmetered, which is why the battery above uses it.

## What is scoped and why

* Calibration is **per subject kind**. Reviewing terminal findings taught the
  system that high-probability "screen-level" findings are often speculation;
  applied to a GUI run, that same lesson suppressed a real flicker detection. A
  terminal verdict must not shape a GUI judgement, so the learned data is now
  keyed by kind (`calibration.json` carries `by_kind`), with the global view used
  only when labels are untagged.
* The GUI sensor runs **where the pixels are**. A 900x560 window is 2 MB per frame
  and a tunnel moved ~1.4 MB/s in testing, which cannot sample a 10 Hz animation.
  The X11 agent samples locally, runs the same `internal/sensors` code, and sends
  change metrics plus the PNGs of frames that changed.

Tier comparison on the fzf pair (same steps, same questions):

* **TypeSafe Jev** found the truncation with a single decision call and stayed
  completely quiet on the fixed build.
* **deepseek-v4.1-flash** (chat) missed it and reported a screen-level false
  positive on the fixed build -- which the review loop then demoted, after one
  label with a note ("post-fix build shows the full value; this was model
  speculation").
* **Qwen3:14b** (self-hosted, offline) needs the deterministic tier to carry it:
  its judgement is weaker, and it is used for the offline claim, not for the
  best-quality numbers.

The review loop changes real output. On the midas TUI, one offline run reported
twelve findings: four "broken border" candidates (midas draws panels with
deliberately open sides), three "column misalignment" candidates (a status bar
that pads columns on purpose), an ellipsis marker, idle-repaint churn from its
live status line, and three clipped-text findings. A twelve-label review pass
with notes -- those panels are open by design, that churn is intended, the
clipping is real and reproducible -- plus one `triage calibrate` reduced the next
run to four findings, keeping the three clipped-text defects and the mixed-evidence
churn, and dropping the rest to below-threshold. Nothing was deleted: the full
set is still in `run.json`, and `argus triage reset --yes` restores stock
behaviour.

Offline evaluation is real, not simulated: `--sandbox-self` re-executes the whole
pipeline inside a loopback-only sandbox, and the run records a canary proving the
analysing process itself could not resolve DNS or reach the internet while it
worked.

## Limits

* The web console is the only UI: the earlier Wails desktop bundle was removed,
  because the analyser often runs somewhere the reviewer is not.
* GUI coverage is X11-based: a native Windows or macOS toolkit (or a browser's
  DOM) would need its own harness. The sensors, question bank, triage and report
  are shared, so the work is the driver plus its structure extraction.
* GUI evidence for issues that only a human can see (clipped words, overlap) is
  judged by a vision model describing a screenshot; the deterministic tier cannot
  see them, and the report says which tier raised each finding.
* Every timing-sensitive conclusion is drawn from observed frames only. Runs
  repeat, but a race-adjacent defect can take several runs to appear.
* Contrast uses the terminal palette Argus assumes (see `screen.DefaultTheme`),
  not the user's real theme.
