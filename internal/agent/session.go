package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/carlvincetan/argus/internal/audit"
	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/harness"
	"github.com/carlvincetan/argus/internal/knowledge"
	"github.com/carlvincetan/argus/internal/provider"
	"github.com/carlvincetan/argus/internal/run"
	"github.com/carlvincetan/argus/internal/screen"
	"github.com/carlvincetan/argus/internal/sensors"
	"github.com/carlvincetan/argus/internal/triage"
)

const (
	stepSettle      = 350 * time.Millisecond
	stepChangeWait  = 1500 * time.Millisecond
	idleObservation = 2200 * time.Millisecond
	startupGrace    = 1200 * time.Millisecond
)

// RunOptions configures one exploration session.
type RunOptions struct {
	Subject        run.Subject
	Cols           int
	Rows           int
	MaxSteps       int
	SandboxProfile string
	UseVision      bool
	AllowTextInput bool
	Personas       []string
	Observer       func(format string, args ...any)
	Isolation      map[string]any
}

// SessionOutcome is the result of a run, including the grids needed to render
// evidence artefacts.
type SessionOutcome struct {
	Session *run.Session
	Grids   []*screen.Grid
}

// RunSession drives the subject and audits what it finds.
func RunSession(
	ctx context.Context,
	cfg config.Config,
	providers provider.Set,
	opts RunOptions,
) (*SessionOutcome, error) {
	log := func(format string, args ...any) {
		if opts.Observer != nil {
			opts.Observer(format, args...)
		}
	}
	cols, rows := opts.Cols, opts.Rows
	if cols == 0 {
		cols = 100
	}
	if rows == 0 {
		rows = 30
	}
	maxSteps := opts.MaxSteps
	if maxSteps == 0 {
		maxSteps = cfg.Limits.MaxSteps
	}
	sandbox := opts.SandboxProfile
	if !cfg.SubjectOffline {
		sandbox = ""
	}

	env := harness.SubjectEnv()
	for name, value := range opts.Subject.Env {
		env = append(env, name+"="+value)
	}
	h, err := harness.Start(harness.Options{
		Argv:           opts.Subject.Argv,
		Cols:           cols,
		Rows:           rows,
		Dir:            opts.Subject.WorkDir,
		Env:            env,
		SandboxProfile: sandbox,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = h.Close() }()

	session := run.NewSession(opts.Subject, providers.Manifest, cfg.Provider, cols, rows)
	session.Isolation = opts.Isolation
	switch {
	case sandbox != "":
		session.AddNote("subject ran under a network-deny sandbox profile")
	case os.Getenv("ARGUS_SANDBOXED") == "1":
		session.AddNote("subject inherited the loopback-only evaluation sandbox (no internet; nested sandbox-exec is not permitted)")
	default:
		session.AddNote("subject ran with network access (sandbox disabled)")
	}
	auditor := NewAuditor(cfg, providers)
	personas, unknownPersonas := knowledge.ResolvePersonas(opts.Personas)
	for _, name := range unknownPersonas {
		session.AddNote("unknown persona ignored: " + name)
	}
	session.AddNote("reviewers: " + personaNames(personas))
	explorer := NewExplorer(cfg, providers)
	explorer.AllowTextInput(opts.AllowTextInput)
	inventory := audit.NewInventory()
	previousGrid := (*screen.Grid)(nil)

	log("waiting for %s to draw (%dx%d)", opts.Subject.Name, cols, rows)
	if !h.WaitIdle(300*time.Millisecond, 8*time.Second) {
		if exited, code := h.Exited(); exited {
			return nil, fmt.Errorf("subject exited during startup with code %d", code)
		}
		session.AddNote("subject never settled at startup; auditing the first stable frame anyway")
	}
	time.Sleep(startupGrace)

	startupGrid := h.Snapshot()
	inventory.ObserveScreen(startupGrid, screen.ScreenName(startupGrid), nil, "startup")
	inventory.ObserveStructure(startupGrid, screen.ScreenName(startupGrid))
	previousGrid = startupGrid
	// Startup audit: the first screen deserves a full look, including the
	// animation behaviour of any spinner that is still running.
	auditObservation(ctx, cfg, auditor, explorer, personas, h, session, AuditInput{
		Grid:    h.Snapshot(),
		Motion:  sensors.AnalyzeMotion(h.Grids(), h.Frames(), h.Inputs(), cfg.Thresholds),
		Checks:  sensors.RunChecks(h.Snapshot(), sensors.DefaultCheckOptions()),
		Action:  "startup",
		History: explorer.History(),
		Subject: opts.Subject,
	}, log)

	for step := 1; step <= maxSteps; step++ {
		if ctx.Err() != nil {
			session.AddNote("run stopped early: " + ctx.Err().Error())
			break
		}
		if exited, code := h.Exited(); exited {
			session.AddNote(fmt.Sprintf("subject exited with code %d after %d steps", code, step-1))
			break
		}
		current := h.Snapshot()
		action, ok := explorer.Next(ctx, current, opts.Subject.Name)
		if !ok || action.Kind == ActionDone {
			session.AddNote(fmt.Sprintf("exploration stopped after %d steps: %s", step-1, action.Label))
			break
		}
		explorer.MarkAttempted(current, action)
		log("step %d/%d: %s", step, maxSteps, action.Label)

		windowStart := h.Now()
		if err := applyAction(h, action); err != nil {
			session.AddNote(fmt.Sprintf("action %q failed: %v", action.Label, err))
			continue
		}
		switch action.Kind {
		case ActionIdle:
			time.Sleep(idleObservation)
			h.WaitIdle(300*time.Millisecond, 2*time.Second)
		case ActionResize:
			h.WaitForChange(stepChangeWait)
			time.Sleep(stepSettle)
		default:
			h.WaitForChange(stepChangeWait)
			h.WaitIdle(stepSettle, 1500*time.Millisecond)
		}

		grids, frames, inputs := windowTraces(h, windowStart)
		after := h.Snapshot()
		checks := sensors.RunChecks(after, sensors.DefaultCheckOptions())
		motion := sensors.AnalyzeMotion(grids, frames, inputs, cfg.Thresholds)
		changed := current.Digest != after.Digest
		if !changed && len(checks) == 0 && len(motion.Evidence) == 0 {
			session.Observations = append(session.Observations, run.Observation{
				ID:       fmt.Sprintf("obs-%d", len(session.Observations)+1),
				T:        after.Time,
				GridID:   after.ID,
				Screen:   screen.ScreenName(after),
				Features: screen.FeatureLabels(after),
				Action:   action.Label,
				Note:     "no visible change",
			})
			continue
		}
		screenName := screen.ScreenName(after)
		inventory.ObserveScreen(after, screenName, previousGrid, action.Label)
		inventory.ObserveStructure(after, screenName)
		if !changed && action.Source == "hint" {
			// The interface advertised this control and using it did nothing.
			inventory.RecordInertAffordance(action.Label, screenName, 1)
		}
		previousGrid = after
		auditObservation(ctx, cfg, auditor, explorer, personas, h, session, AuditInput{
			Grid:    after,
			Motion:  motion,
			Checks:  checks,
			Action:  action.Label,
			History: explorer.History(),
			Subject: opts.Subject,
		}, log)
	}

	// Final idle observation: animation defects need a still screen to show up.
	log("final idle observation for animation defects")
	windowStart := h.Now()
	time.Sleep(idleObservation)
	idleGrids, idleFrames, idleInputs := windowTraces(h, windowStart)
	final := h.Snapshot()
	inventory.ObserveScreen(final, screen.ScreenName(final), previousGrid, "idle observation")
	inventory.ObserveStructure(final, screen.ScreenName(final))
	auditObservation(ctx, cfg, auditor, explorer, personas, h, session, AuditInput{
		Grid:    final,
		Motion:  sensors.AnalyzeMotion(idleGrids, idleFrames, idleInputs, cfg.Thresholds),
		Checks:  sensors.RunChecks(final, sensors.DefaultCheckOptions()),
		Action:  "idle observation",
		History: explorer.History(),
		Subject: opts.Subject,
	}, log)

	// Feature audit: what does the application offer, and what is missing from
	// what it offers? This is the run-level reviewer, not a per-screen one.
	log("feature audit over %d screens and %d observed features", len(inventory.Screens), len(inventory.Hits))
	runFeatureAudit(ctx, auditor, personas, session, inventory, opts.Subject, log)

	if len(inventory.Hits) > 0 || len(inventory.Screens) > 0 {
		session.Inventory = inventory
	}
	result := h.Result()
	calibration := auditor.Calibration()
	session.Isolation = isolationWithCalibration(session.Isolation, calibration)
	session.Findings = auditor.Findings()
	session.Rejected = auditor.Rejected()
	session.Finalise(result.Duration, result.FrameCount, result.InputCount)
	decisionCalls, visionCalls, tokens, cost := auditor.Stats()
	exploreCalls, exploreTokens, exploreCost := explorer.Usage()
	session.Cost = run.Cost{
		DecisionCalls: decisionCalls + exploreCalls,
		VisionCalls:   visionCalls,
		InputTokens:   tokens + exploreTokens,
		CostUSD:       cost + exploreCost,
		Local:         cfg.Provider == "local",
	}
	session.AddLog("finished: %d findings from %d observations", len(session.Findings), len(session.Observations))
	log("done: %d findings, %d decision calls, $%.5f", len(session.Findings), session.Cost.DecisionCalls, session.Cost.CostUSD)
	return &SessionOutcome{Session: session, Grids: h.Grids()}, nil
}

// isolationWithCalibration records which calibration data was in force, so a run
// can be reproduced (or explained) after the label history moves on.
func isolationWithCalibration(existing map[string]any, calibration triage.Calibration) map[string]any {
	out := map[string]any{}
	for key, value := range existing {
		out[key] = value
	}
	out["calibration"] = map[string]any{
		"summary":      calibration.Summary(),
		"labels":       calibration.Labels,
		"generated_at": calibration.GeneratedAt,
		"suppressed":   calibration.Suppressed,
		"promoted":     calibration.Promoted,
	}
	return out
}

func applyAction(h *harness.Harness, action Action) error {
	switch action.Kind {
	case ActionKey:
		return h.SendKey(action.Key)
	case ActionText:
		return h.SendText(action.Text)
	case ActionResize:
		return h.Resize(action.Size[0], action.Size[1])
	case ActionRedraw:
		return h.SendKey("CTRL+L")
	case ActionIdle:
		return nil
	default:
		return fmt.Errorf("unknown action kind %q", action.Kind)
	}
}

// windowTraces returns the grids, frames and inputs observed since a timestamp.
func windowTraces(h *harness.Harness, since time.Duration) ([]*screen.Grid, []harness.FrameEvent, []harness.InputEvent) {
	grids := []*screen.Grid{}
	for _, grid := range h.Grids() {
		if grid.Time >= since {
			grids = append(grids, grid)
		}
	}
	if len(grids) == 0 {
		if latest := h.Snapshot(); latest != nil {
			grids = append(grids, latest)
		}
	}
	frames := []harness.FrameEvent{}
	for _, frame := range h.Frames() {
		if frame.T >= since {
			frames = append(frames, frame)
		}
	}
	inputs := []harness.InputEvent{}
	for _, input := range h.Inputs() {
		if input.T >= since {
			inputs = append(inputs, input)
		}
	}
	return grids, frames, inputs
}

// auditObservation runs every configured persona over one observation and
// records it. Personas are narrow reviewers, so several run over the same
// evidence and their findings are merged.
func auditObservation(
	ctx context.Context,
	cfg config.Config,
	auditor *Auditor,
	explorer *Explorer,
	personas []knowledge.Persona,
	h *harness.Harness,
	session *run.Session,
	input AuditInput,
	log func(format string, args ...any),
) {
	if input.Grid == nil {
		return
	}
	input.Features = screen.FeatureLabels(input.Grid)
	outcome := auditWithPersonas(ctx, auditor, personas, input)
	_ = explorer
	_ = h
	observation := run.Observation{
		ID:         fmt.Sprintf("obs-%d", len(session.Observations)+1),
		T:          input.Grid.Time,
		GridID:     input.Grid.ID,
		Screen:     screen.ScreenName(input.Grid),
		Features:   input.Features,
		Action:     input.Action,
		Note:       fmt.Sprintf("%d candidates, %d findings", outcome.Candidates, len(outcome.Findings)),
		ModelCalls: outcome.DecisionCalls,
		Cost:       outcome.CostUSD,
		Candidates: outcome.CandidateTraces,
	}
	if outcome.Error != nil {
		observation.Error = outcome.Error.Error()
		observation.Note = "audit failed"
	}
	session.Observations = append(session.Observations, observation)
	if outcome.Error != nil {
		session.AddLog("audit error: %v", outcome.Error)
		log("audit error: %v", outcome.Error)
		return
	}
	for _, finding := range outcome.Findings {
		log("finding [%s/%s] %s: %s", finding.SeverityName, finding.Verified, finding.Title, finding.Brief)
	}
}

// auditWithPersonas asks each persona about the same evidence, in parallel, and
// merges the results. Findings that several personas raise are deduplicated by
// the auditor's merge step.
func auditWithPersonas(
	ctx context.Context,
	auditor *Auditor,
	personas []knowledge.Persona,
	input AuditInput,
) AuditResult {
	if len(personas) == 0 {
		return auditor.Audit(ctx, input)
	}
	type reply struct {
		persona knowledge.Persona
		outcome AuditResult
	}
	replies := make(chan reply, len(personas))
	var wait sync.WaitGroup
	for _, persona := range personas {
		if persona.Completeness {
			continue // runs once per session, not per screen
		}
		wait.Add(1)
		go func(persona knowledge.Persona) {
			defer wait.Done()
			auditor.mu.Lock()
			auditor.SetPersona(persona)
			outcome := auditor.Audit(ctx, input)
			auditor.mu.Unlock()
			replies <- reply{persona: persona, outcome: outcome}
		}(persona)
	}
	wait.Wait()
	close(replies)

	merged := AuditResult{RawAnswers: map[string]float64{}, CandidateTraces: []run.CandidateTrace{}}
	for item := range replies {
		merged.DecisionCalls += item.outcome.DecisionCalls
		merged.Candidates += item.outcome.Candidates
		merged.InputTokens += item.outcome.InputTokens
		merged.CostUSD += item.outcome.CostUSD
		merged.Findings = append(merged.Findings, item.outcome.Findings...)
		merged.Rejected = append(merged.Rejected, item.outcome.Rejected...)
		merged.CandidateTraces = append(merged.CandidateTraces, item.outcome.CandidateTraces...)
		if item.outcome.Error != nil {
			merged.Error = item.outcome.Error
		}
		for id, value := range item.outcome.RawAnswers {
			merged.RawAnswers[id] = value
		}
	}
	return merged
}

// runFeatureAudit runs the completeness persona once per session, over the
// inventory the run built up.
func runFeatureAudit(
	ctx context.Context,
	auditor *Auditor,
	personas []knowledge.Persona,
	session *run.Session,
	inventory *audit.Inventory,
	subject run.Subject,
	log func(format string, args ...any),
) {
	completeness, ok := completenessPersona(personas)
	if !ok {
		return
	}
	auditor.mu.Lock()
	auditor.SetPersona(completeness)
	outcome := auditor.AuditInventory(ctx, inventory, subject)
	auditor.mu.Unlock()
	if outcome.Error != nil {
		session.AddLog("feature audit error: %v", outcome.Error)
		log("feature audit error: %v", outcome.Error)
		return
	}
	session.Observations = append(session.Observations, run.Observation{
		ID:         fmt.Sprintf("obs-%d", len(session.Observations)+1),
		Screen:     "app:" + subject.Name,
		Action:     "feature audit",
		Note:       fmt.Sprintf("%d candidates, %d findings", outcome.Candidates, len(outcome.Findings)),
		ModelCalls: outcome.DecisionCalls,
		Cost:       outcome.CostUSD,
		Candidates: outcome.CandidateTraces,
	})
	for _, finding := range outcome.Findings {
		log("finding [%s/%s] %s: %s", finding.SeverityName, finding.Verified, finding.Title, finding.Brief)
	}
}

func completenessPersona(personas []knowledge.Persona) (knowledge.Persona, bool) {
	for _, persona := range personas {
		if persona.Completeness {
			return persona, true
		}
	}
	return knowledge.Persona{}, false
}

func personaNames(personas []knowledge.Persona) string {
	names := make([]string, 0, len(personas))
	for _, persona := range personas {
		names = append(names, persona.ID)
	}
	return strings.Join(names, ", ")
}
