# Argus as the Polymux tester (plan, 12 Oct 2026)

Owner: Cody. Argus becomes the TESTER role in the coding pipeline (implementer = Codex/Cursor/OpenCode, reviewer = a separate session). Code is written by coding agents; this file is the plan.

North star: simple, efficient, very fast; judged by how quickly and reliably a full UI test run finishes. Rust port: not now (see Cody's argus-rust-verdict, 12 Oct 2026: Go CPU is ~10% of wall; fix waits, sessions, parallelism, change-awareness first). Every run records three numbers in `var/metrics.tsv`: full-surface wall time, simplicity (Go LOC + number of moving parts: processes, models, hosts), and bug-catch rate (seeded + known regressions caught / total).

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
- S2: macOS AX driver + deterministic checks on 5 core screens. Done: seeded clipping/overlap bugs caught 5/5, full pass < 3 min.
- S3: iOS simulator lane + change-aware ordering. Done: same seeded set caught on iOS; changed-area pass < 60 s.
- S4: behaviour flows (reload, reconnect, unread, routines, delegation, memory) + baseline diffs. Done: catch rate >= 90% on a 20-bug seeded set.
- B1: benchmark corpus + `argus bench` + history.tsv (see Benchmark). Done: one command prints full-run time, caught/planted and false positives, and appends a history row.
- S5: model tier only where S1-S4 leave ambiguity; measure added time vs catch-rate gain, keep only if it pays.
