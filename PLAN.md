# Argus as the Polymux tester (plan, 12 Oct 2026)

Owner: Cody. Argus becomes the TESTER role in the coding pipeline (implementer = Codex/Cursor/OpenCode, reviewer = a separate session). Code is written by coding agents; this file is the plan.

North star: simple, efficient, very fast. Every run records three numbers in `var/metrics.tsv`: full-surface wall time, simplicity (Go LOC + number of moving parts: processes, models, hosts), and bug-catch rate (seeded + known regressions caught / total).

## What Argus is today
Go pipeline (cmd/argus, internal/*): pty harness -> terminal world model -> 10 deterministic cell-grid checks + frame-ledger animation analysis (flicker, repaint churn, stuck spinners, drift) -> decision tier (Jev/OpenAI-compatible) -> blinded vision tier -> triage -> backlog with evidence (`run.json`, stills, video). An X11 agent drives Linux GUIs; an audit package infers capabilities and missing companions. Runs headless; offline mode with a local llama.cpp/ollama model.

## Gaps for Polymux
1. No macOS/iOS driver: it can drive a pty or X11 only, not SwiftUI.
2. No pixel-level world model for native screens (it works on a cell grid); it needs the accessibility tree + screenshots instead.
3. No server/agent-behaviour lane: replies, delegation, memory and routines are not covered.
4. No coverage map and no change-aware ordering.
5. No scripted flows/fixtures (seeded data, reload, reconnect, fake clock).
6. Not wired into goal-loop; no metrics file.

## Drivers (decision: cvmac now; Xcode on cvmini recommended)
- macOS app: build with `xcodebuild`, launch, drive through the Accessibility API (AXUIElement tree for screens, components, states; synthetic events for interaction) plus `screencapture` for pixels and short video for motion. New `internal/harness/macos` (small Swift helper binary + Go client), mirroring the x11-agent protocol.
- iOS: `xcrun simctl` boots and installs; XCUITest-free driving through `simctl io screenshot/recordVideo` + an accessibility dump from a UI-test runner target (XCTest is the only supported tap/scroll path on the simulator).
- Both need Xcode. cvmac has Xcode 27.0 with iOS 26.5/27.0 simulators but is a laptop that sleeps; cvmini (always on, where goal-loop runs) has no Xcode. Recommendation: install Xcode on cvmini (about 40 GB disk, one Apple ID sign-in, license accept with sudo). Until then the UI lane runs on cvmac via `pending-add` and is reported as "not run" when cvmac is away.
- Go server / agent behaviour: reuse `apps/server/evals` (fake model emulator) as the behaviour lane; Argus calls it and adds flows for delegation, memory, routines, reconnect and reload against a real server process with a scripted model.
- Web (site): Playwright headless on cvmini, same check set on DOM + screenshots.

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

## Stages (each done = checkable)
- S0 (done 12 Oct): repo under git, paths fixed.
- S1: metrics.tsv + coverage.json + `argus polymux` server lane wrapping evals. Done: tester.sh calls it; full-surface server pass < 60 s.
- S2: macOS AX driver + deterministic checks on 5 core screens. Done: seeded clipping/overlap bugs caught 5/5, full pass < 3 min.
- S3: iOS simulator lane + change-aware ordering. Done: same seeded set caught on iOS; changed-area pass < 60 s.
- S4: behaviour flows (reload, reconnect, unread, routines, delegation, memory) + baseline diffs. Done: catch rate >= 90% on a 20-bug seeded set.
- S5: model tier only where S1-S4 leave ambiguity; measure added time vs catch-rate gain, keep only if it pays.
