# Argus as the Polymux tester (plan, 12 Oct 2026)

Objective (1 Nov 2026, overrides anything below that conflicts): a very powerful tool for maintaining UI/UX, built for agents and humans to use. Argus should map an app's full state, including its state diagram, understand what is right and wrong with its UI/UX, and discover problems in its screens and user flows autonomously. Polymux is a subject, not a dependant: Polymux never requires Argus.

Owner: Cody. Argus becomes the TESTER role in the coding pipeline (implementer = Codex/Cursor/OpenCode, reviewer = a separate session). Code is written by coding agents; this file is the plan.

Constraint: simple, efficient, very fast; a full UI test run must finish quickly and reliably. Rust port: not now (see Cody's argus-rust-verdict, 12 Oct 2026: Go CPU is ~10% of wall; fix waits, sessions, parallelism, change-awareness first). Every run records three numbers in `var/metrics.tsv`: full-surface wall time, simplicity (Go LOC + number of moving parts: processes, models, hosts), and bug-catch rate (seeded + known regressions caught / total).

## What Argus is today
Go pipeline (cmd/argus, internal/*): pty harness -> terminal world model -> 10 deterministic cell-grid checks + frame-ledger animation analysis (flicker, repaint churn, stuck spinners, drift) -> decision tier (Jev/OpenAI-compatible) -> blinded vision tier -> triage -> backlog with evidence (`run.json`, stills, video). An X11 agent drives Linux GUIs; an audit package infers capabilities and missing companions. Runs headless; offline mode with a local llama.cpp/ollama model.

## Gaps for Polymux
1. No macOS/iOS driver: it can drive a pty or X11 only, not SwiftUI.
2. No pixel-level world model for native screens (it works on a cell grid); it needs the accessibility tree + screenshots instead.
3. No server/agent-behaviour lane: replies, delegation, memory and routines are not covered.
4. No coverage map and no change-aware ordering.
5. No scripted flows/fixtures (seeded data, reload, reconnect, fake clock).
6. Not wired into goal-loop; no metrics file.

## Drivers (target: cvmini; cvmac only as a temporary fallback)
Carlvince is moving off cvmac. The UI lane targets cvmini, which gets Xcode and Screen Recording if he approves (Chief is asking, 12 Oct 2026). cvmini is always on and already runs goal-loop, so build, drive and capture happen on the same host as the tester.
- macOS app: `python3 scripts/fetch_cua.py`, then `xcodebuild` (FlareHQ 23YB4896XA signing), launch, drive through the Accessibility API (AXUIElement tree for screens, components, states; synthetic events for interaction), `screencapture` for pixels and `screencapture -v` for motion. New `internal/harness/macos` (small Swift helper + Go client), mirroring the x11-agent protocol. Needs Screen Recording and Accessibility for the helper on cvmini.
- iOS: `xcrun simctl` boots/installs; driving through an XCTest UI-runner target (the only supported tap/scroll path on the simulator) plus `simctl io screenshot/recordVideo`.
- Until Xcode is on cvmini: headless ImageRenderer snapshots and the macOS build run on cvmac (Xcode 27, iOS 26.5/27 simulators) when it is awake; otherwise the UI lane reports "not run" and never passes a UI unit.
- Go server / agent behaviour: reuse `apps/server/evals` (fake model emulator) as the behaviour lane; Argus calls it and adds flows for delegation, memory, routines, reconnect and reload against a real server process with a scripted model.
- Web (site): headless Chromium over CDP on cvmini (goal-loop site-shots.mjs), same check set on DOM + screenshots.

## Coverage map
`coverage.yaml` in the Polymux repo (versioned with the app):
```
screens:   - id: chat.thread   platforms: [mac, ios]   states: [empty, loading, error, populated, unread]   source: [apps/Polymux/Sources/Chat/*]
flows:     - id: send-reply    steps: [open chat.thread, type, send, wait reply]   checks: [layout, timing, persistence-after-reload]
behaviour: - id: B4-stop-all   lane: server   eval: TestB4
```
Each run writes `coverage.json`: per screen/state/flow last result, time and gaps (declared but never reached; reached but undeclared). Change-aware ordering maps the diff's files to `source:` globs, runs those items first, then the quick full-surface pass.

## Checks
1. Deterministic first (no model): AX-tree layout (overlap, clipping, offscreen, truncation), contrast, empty/error/loading states present, scroll position and unread counts after actions, timing budgets, state equality after reload/reconnect, frame-diff animation metrics, screenshot diff against the last approved baseline.
2. Model calls only for: ambiguous visual candidates (blinded vision description) and judging agent reply quality. Default: local open model on a loopback host (offline, free); hosted fallback needs one gateway key from the vault (OpenRouter or Command Code), never in prompts or the repo.

## goal-loop integration
`tester.sh` stages: scope check -> exit predicate -> Go verify -> `argus polymux --changed <files> --quick --out <dir>` (exit 0/1, prints `ARGUS: PASS|FAIL n findings, t s`). UI lane: called only when Xcode is present on the host; otherwise it records "UI lane not run (no Xcode)" and never passes a UI unit.

## Benchmark (how we know Argus got faster or better)
A fixed corpus of Polymux builds, versioned in `benchmark/polymux/`:
- Clean builds: 3 pinned commits of Polymux main (server + macOS app + site) with no known defects.
- Planted builds: each = a clean commit + one small patch that plants one known bug, with a manifest entry (id, layer, expected finding class, screen/flow). Target 30 at first: UI (clipping/overflow, overlap, contrast, missing empty/error state, broken hover/focus/selected state, flicker, stuck spinner, layout drift), behaviour (unread count wrong after reload, scroll jump, lost state after reconnect, race on fast double-send), agent (dropped reply, delegation not routed, memory not recalled, routine failure silent).
- One command: `argus bench polymux --corpus benchmark/polymux --out var/bench/<date>` builds/starts each build, runs the full-surface pass, and scores against the manifest.
- Output: per run, appended to `benchmark/history.tsv` (never rewritten): date, argus commit, host, full-run wall seconds (median of 3), per-lane seconds, bugs caught / planted, false positives on clean builds, flaky items (differing across the 3 runs), Go LOC + moving parts, model calls and cost.
- Regression gate: a change to Argus may not lower bugs caught, raise false positives, or slow the full run by more than 10% vs the last history row without a stated reason in its PR.
- Stage B1 (after S1): build the corpus format, `argus bench`, history.tsv and the first 5 planted server/agent bugs (runnable on cvmini now); UI planted bugs follow once the macOS lane exists (S2).

## Stages (each done = checkable)
- S0 (done 12 Oct): repo under git, paths fixed.
- S1: metrics.tsv + coverage.json + `argus polymux` server lane wrapping evals. Done: tester.sh calls it; full-surface server pass < 60 s.
- S2: macOS AX driver + deterministic checks on 5 core screens. Done: seeded clipping/overlap bugs caught 5/5, full pass < 3 min. The driver uses Polymux's Control helper rather than a new Swift helper (`internal/harness/control.go`); `argus polymux ui` runs it (RUN.md section 6). Seeded on the real app: overlap, clipped control, unnamed control, inert control and crash all caught (RUN.md sections 6 and 7 cover replay and the intent ledger). Still open: the 5-core-screens coverage map, the corpus entries in `benchmark/polymux/`, and a model-tier run on OpenCode Go.
- S3: iOS simulator lane + change-aware ordering. Done: same seeded set caught on iOS; changed-area pass < 60 s. Change-aware selection exists (RUN.md section 6): area rules, baseline links that add journeys the map forgot, and a fail-open decision `--filter`. Measured with Jev on OpenCode (RUN.md section 6): full recall on planted changes, modest savings, and none on real commits once server-only changes stopped planning journeys; it stays optional. The harness plants label renames and nearby layout edits in a scratch diff and checks the journeys showing that label survive (`go run ./var/exp/filtereval -provider opencode`, kept local). It needs a working OpenCode key (`opencode auth login`), a Command Code key or OpenRouter credits.
- S4: behaviour flows (reload, reconnect, unread, routines, delegation, memory) + baseline diffs. Done: catch rate >= 90% on a 20-bug seeded set.
- B1: benchmark corpus + `argus bench` + history.tsv (see Benchmark). Done: one command prints full-run time, caught/planted and false positives, and appends a history row.
- S5: model tier only where S1-S4 leave ambiguity; measure added time vs catch-rate gain, keep only if it pays.

## UI/UX coverage roadmap (31 Oct 2026)
Done so far is in RUN.md sections 6 to 8, including drift baselines, which answer commit 59312ed: a bug-fix pass replaced New Bot's inline name field and added a Model row unasked, and no defect check could flag it. Next, roughly in order of value. Ideas come from pstack (MIT), gstack (MIT), Emil Kowalski's skills (MIT) and Impeccable (Apache-2.0); no code is copied.
1. Frame recording: a 60 fps stream per journey, to catch flashes under 250 ms, measure hitch ratio (Apple's bands: under 5 ms/s is good, 5-10 is noticeable) and compare transition times with Emil's table (press 100-160 ms, popover 125-250 ms, modal 200-500 ms). Today's capture tops out near 6 fps. First target: glass that is wrong only mid-transition. In Polymux `.glassEffect` ignored an ancestor's slide, so the selected `PillPicker` pill sat at its final position while the side panel took 0.22 s to arrive, and every settled screenshot looked right. The detector is generic: compare each transition frame with the settled frame, and flag tiles that already match it while the rest of their container is still moving, which means something arrived early. Suspects are glass controls inside anything with `.transition`, `.offset`, `.opacity` or `withAnimation` on an ancestor: panels, modals, dropdowns, sheets. Proof case: the code before `EdgeSlide` and `SlideOffset` in `Motion.swift`. A slowed `Motion.columnTime` helps by hand, but a build flag for it would have to be added.
2. Performance baselines, by pstack's benchmark checklist: state the claim first, 5 alternating runs per side, median and range, name the limiter, count errors, and call it inconclusive otherwise. Add cold against warm opens to test preloading and caching, and Polymux's `--verify-renders` counts to prove a gesture stays local.
3. Side effects beyond the window: state equal after relaunch and reconnect, requests per action, real-server journeys.
4. DESIGN.md as checks: empty states centred, no retry buttons, one-word visible labels, one quiet status line. Rules a machine can measure should stop being prose.
5. State model: declared invariants over `states.mmd`, then dead ends and focus traps, with pstack's TLA+ statechart for the flows worth proving.
6. Feature map: one file per user-facing feature, kept honest like pstack's maintain-verification-skill, replacing the planned `coverage.yaml`.
7. Model review: gstack's QA taxonomy and design checklist as typed questions, with Design and AI Slop scores, once OpenCode Go works. It needs the `x-opencode-session` header; the URL is in Polymux's `apps/server/.env`.
8. iOS lane with mobile-native's rules: 44 pt targets, safe areas, the keyboard, Dynamic Type, Reduce Motion, landscape. Touch feel stays a real-device check.
9. Web lane for the site: CSS checks for `100vh`, ungated hover, inputs under 16 px, disabled zoom, one `theme-color`, `transition: all`, plus Impeccable's slop rules.
