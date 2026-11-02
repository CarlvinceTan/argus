package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/carlvincetan/argus/internal/audit"
	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/harness"
	"github.com/carlvincetan/argus/internal/jev"
	"github.com/carlvincetan/argus/internal/knowledge"
	"github.com/carlvincetan/argus/internal/provider"
	"github.com/carlvincetan/argus/internal/run"
	"github.com/carlvincetan/argus/internal/sensors"
)

const (
	guiObserveFrames  = 8
	guiObserveSpacing = 110 * time.Millisecond
	guiSettle         = 250 * time.Millisecond
	guiAnalyzeWindow  = 1400 * time.Millisecond
	guiAnalyzeSpacing = 55 * time.Millisecond
)

// pixelAnalyzer is implemented by harnesses that measure on their own host (the
// X11 agent). Sampling a 10 Hz animation across a network link aliases it, so the
// fast sensor runs where the pixels are and returns its conclusions.
type pixelAnalyzer interface {
	Analyze(duration, interval time.Duration) ([]sensors.Evidence, []*harness.X11Frame, error)
}

// guiMotion produces the pixel motion report for one observation window.
func guiMotion(h harness.GUIHarness, cfg config.Config, since time.Time) sensors.MotionReportGUI {
	if analyzer, ok := h.(pixelAnalyzer); ok {
		evidence, frames, err := analyzer.Analyze(guiAnalyzeWindow, guiAnalyzeSpacing)
		if err == nil {
			report := sensors.MotionReportGUI{Frames: len(frames), Evidence: evidence}
			if len(frames) >= 2 {
				report.DurationMS = (frames[len(frames)-1].T - frames[0].T).Milliseconds()
			}
			for _, item := range evidence {
				if item.Measured > report.IdleChangedPixelRate {
					report.IdleChangedPixelRate = item.Measured
				}
			}
			return report
		}
	}
	frames := framesInWindow(h.Frames(), since)
	return sensors.AnalyzePixels(frames, h.Inputs(), cfg.Thresholds)
}

// GUIRunOptions configures a GUI session.
type GUIRunOptions struct {
	Subject     run.Subject
	Display     string
	AgentAddr   string
	WindowMatch string
	// URL turns the session into a browser session: the subject is a web page
	// driven through the DevTools protocol instead of an X11 window.
	URL            string
	Browser        string
	BrowserArgs    []string
	Cols           int
	Rows           int
	InitialActions []guiAction
	MaxSteps       int
	SandboxProfile string
	UseVision      bool
	AllowClicks    bool
	Personas       []string
	Observer       func(format string, args ...any)
	Isolation      map[string]any
}

// GUISessionOutcome is the result of a GUI run, including frames for evidence.
type GUISessionOutcome struct {
	Session *run.Session
	Frames  []*harness.X11Frame
}

// guiAction is one thing the GUI explorer can do.
type guiAction struct {
	ID    string
	Kind  string
	Label string
	Key   string
	Point [2]int
	Size  [2]int
	URL   string
	// Native actions target an element by key instead of a point.
	Element string
	Text    string
	// Group is the part of the window an action belongs to, so a sweep with no
	// model visits every part instead of walking one long list.
	Group string
}

// buildGUIActions derives a bounded action space from the window size. GUI
// programs advertise nothing in the pixels, so unlike the terminal explorer this
// space is heuristic: keys that switch views, clicks on a coarse grid, window
// resizes, and waiting.
func buildGUIActions(width, height int, allowClicks bool) []guiAction {
	actions := []guiAction{}
	keys := []struct {
		key   string
		label string
	}{
		{"TAB", "move focus with Tab"},
		{"DOWN", "move the selection down"},
		{"UP", "move the selection up"},
		{"ENTER", "activate the focused control"},
		{"ESC", "dismiss whatever is open"},
		{"1", "switch to the first view"},
		{"2", "switch to the second view"},
		{"3", "switch to the third view"},
		{"d", "open the dropdown menu"},
		{"SPACE", "toggle the focused control"},
		{"?", "ask for help"},
		{"/", "start a search"},
	}
	for _, entry := range keys {
		actions = append(actions, guiAction{
			ID:    "key:" + entry.key,
			Kind:  "key",
			Key:   entry.key,
			Label: "press " + entry.key + " to " + entry.label,
		})
	}
	if allowClicks {
		for _, fx := range []float64{0.25, 0.6, 0.85} {
			for _, fy := range []float64{0.2, 0.55, 0.85} {
				x, y := int(float64(width)*fx), int(float64(height)*fy)
				actions = append(actions, guiAction{
					ID:    fmt.Sprintf("click:%d,%d", x, y),
					Kind:  "click",
					Point: [2]int{x, y},
					Label: fmt.Sprintf("click at %d,%d and see what it does", x, y),
				})
			}
		}
	}
	for _, size := range [][2]int{{1100, 700}, {640, 420}, {width, height}, {width + 200, height + 120}} {
		if size[0] < 320 || size[1] < 240 {
			continue
		}
		actions = append(actions, guiAction{
			ID:    fmt.Sprintf("resize:%dx%d", size[0], size[1]),
			Kind:  "resize",
			Size:  size,
			Label: fmt.Sprintf("resize the window to %dx%d to test its layout", size[0], size[1]),
		})
	}
	actions = append(actions,
		guiAction{ID: "redraw", Kind: "redraw", Label: "wait for the window to settle and watch its animation"},
		guiAction{ID: "idle", Kind: "idle", Label: "leave it alone and watch for flicker or idle repainting"},
	)
	return actions
}

// RunGUISession drives a GUI program and audits what it sees.
func RunGUISession(
	ctx context.Context,
	cfg config.Config,
	providers provider.Set,
	opts GUIRunOptions,
) (*GUISessionOutcome, error) {
	log := func(format string, args ...any) {
		if opts.Observer != nil {
			opts.Observer(format, args...)
		}
	}
	display := opts.Display
	if display == "" {
		display = os.Getenv("ARGUS_GUI_DISPLAY")
	}
	if display == "" && opts.AgentAddr == "" && opts.URL == "" {
		return nil, fmt.Errorf("gui mode needs either --display (local X server), --x11-agent (agent next to the subject) or --url (browser)")
	}
	if opts.AgentAddr != "" {
		// The subject runs on the machine with the X server, so its DISPLAY is
		// local there; the analyser talks to the agent instead.
		display = os.Getenv("ARGUS_GUI_SUBJECT_DISPLAY")
		if display == "" {
			display = ":99"
		}
	}
	maxSteps := opts.MaxSteps
	if maxSteps == 0 {
		maxSteps = 10
	}
	env := harness.SubjectEnv()
	if opts.AgentAddr != "" {
		// The subject is launched remotely: the caller passes a command that
		// already runs on the GUI host (for example through ssh), so DISPLAY is
		// whatever that host uses.
		if remote := opts.Subject.Env["DISPLAY"]; remote != "" {
			env = append(env, "DISPLAY="+remote)
		}
	} else {
		env = append(env, "DISPLAY="+display)
	}
	for name, value := range opts.Subject.Env {
		env = append(env, name+"="+value)
	}
	// In web mode the subject is a page served elsewhere: there is no process to
	// launch, and the browser is the thing being driven.
	var command *exec.Cmd
	if opts.URL == "" {
		argv := append([]string{}, opts.Subject.Argv...)
		if opts.SandboxProfile != "" && opts.AgentAddr == "" {
			argv = append([]string{"/usr/bin/sandbox-exec", "-f", opts.SandboxProfile, "--"}, argv...)
		}
		if len(argv) == 0 {
			return nil, fmt.Errorf("gui mode needs a subject command (--cmd)")
		}
		command = exec.Command(argv[0], argv[1:]...)
		command.Dir = opts.Subject.WorkDir
		command.Env = env
		command.Stdout = os.Stderr
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			return nil, fmt.Errorf("starting GUI subject: %w", err)
		}
		defer func() {
			if command.Process != nil {
				_ = command.Process.Kill()
			}
		}()
	}

	var harness_ harness.GUIHarness
	var domObserver harness.DOMObserver
	if opts.URL != "" {
		browser, err := harness.StartCDP(harness.CDPOptions{
			Binary:    opts.Browser,
			URL:       opts.URL,
			Width:     opts.Cols,
			Height:    opts.Rows,
			ExtraArgs: opts.BrowserArgs,
		})
		if err != nil {
			return nil, err
		}
		harness_ = browser
		domObserver = browser
	} else {
		started, err := harness.OpenGUI(opts.AgentAddr, display, opts.WindowMatch)
		if err != nil {
			return nil, err
		}
		harness_ = started
		if observer, ok := started.(harness.DOMObserver); ok {
			domObserver = observer
		}
	}
	defer func() { _ = harness_.Close() }()
	if refinder, ok := harness_.(harness.WindowRefinder); ok {
		// The subject was just launched, so any window the harness found before
		// this point belongs to a previous run.
		if err := refinder.Refind(); err != nil {
			return nil, err
		}
	}

	session := run.NewSession(opts.Subject, providers.Manifest, cfg.Provider, 0, 0)
	session.Kind = run.KindGUI
	session.Isolation = opts.Isolation
	width, height := harness_.Size()
	session.Terminal = [2]int{width, height}
	switch {
	case opts.SandboxProfile != "" && opts.AgentAddr == "":
		session.AddNote("subject ran under a network-deny sandbox profile")
	case opts.AgentAddr != "":
		session.AddNote("subject ran on the GUI host; the analyser reached it through the X11 agent")
	case os.Getenv("ARGUS_SANDBOXED") == "1":
		session.AddNote("subject inherited the loopback-only evaluation sandbox")
	}
	if opts.AgentAddr != "" {
		session.AddNote(fmt.Sprintf("GUI subject observed through the X11 agent at %s (display %s)", opts.AgentAddr, display))
	} else {
		session.AddNote(fmt.Sprintf("GUI subject driven over X11 display %s", display))
	}

	auditor := NewAuditor(cfg, providers)
	auditor.SetKind(run.KindGUI)
	personas, unknownPersonas := knowledge.ResolvePersonas(opts.Personas)
	for _, name := range unknownPersonas {
		session.AddNote("unknown persona ignored: " + name)
	}
	session.AddNote("reviewers: " + personaNames(personas))
	explorer := newGUIExplorer(cfg, providers)
	inventory := audit.NewInventory()

	// Settle, then take the first measurement window.
	harness_.WaitIdle(guiSettle, 3*time.Second)
	time.Sleep(300 * time.Millisecond)
	description := ""
	_ = description
	if startup := auditGUIState(ctx, cfg, auditor, session, inventory, GUIAuditInput{
		Frame:   mustCapture(harness_),
		Strip:   harness_.Observe(3, guiObserveSpacing),
		Motion:  guiMotion(harness_, cfg, time.Now().Add(-guiAnalyzeWindow)),
		Action:  "startup",
		Subject: opts.Subject,
	}, log); startup != nil && inventory != nil {
		inventory.ObserveText(startup.description, "gui:"+opts.Subject.Name, 0)
	}

	actions := buildGUIActions(width, height, opts.AllowClicks)
	if domObserver != nil {
		if snapshot, err := domObserver.ObserveDOM(); err == nil {
			observeDOM(inventory, snapshot, "web:"+opts.Subject.Name, 0)
			actions = webActions(snapshot, width, height)
			session.AddNote(fmt.Sprintf("page structure: %d interactive elements, %d rows", len(snapshot.Elements), snapshot.Rows))
		}
	}
	for _, action := range opts.InitialActions {
		if err := applyGUIAction(harness_, action); err != nil {
			session.AddNote(fmt.Sprintf("initial action %q failed: %v", action.Label, err))
		}
		harness_.WaitIdle(guiSettle, 2*time.Second)
	}

	for step := 1; step <= maxSteps; step++ {
		if ctx.Err() != nil {
			session.AddNote("run stopped early: " + ctx.Err().Error())
			break
		}
		if command != nil && command.ProcessState != nil && command.ProcessState.Exited() {
			session.AddNote("subject exited during the run")
			break
		}
		action, ok := explorer.next(ctx, actions, description, step)
		if !ok {
			session.AddNote("exploration stopped: no action left to try")
			break
		}
		log("step %d/%d: %s", step, maxSteps, action.Label)
		start := time.Now()
		if err := applyGUIAction(harness_, action); err != nil {
			session.AddNote(fmt.Sprintf("action %q failed: %v", action.Label, err))
			continue
		}
		if action.Kind == "idle" || action.Kind == "redraw" {
			time.Sleep(1200 * time.Millisecond)
		} else {
			harness_.WaitForChange(1200 * time.Millisecond)
			harness_.WaitIdle(guiSettle, 2*time.Second)
		}
		window := framesInWindow(harness_.Frames(), start)
		strip := sampleFramesG(window, 4)
		frame := mustCapture(harness_)
		motion := guiMotion(harness_, cfg, start)
		result := auditGUIState(ctx, cfg, auditor, session, inventory, GUIAuditInput{
			Frame:   frame,
			Strip:   strip,
			Motion:  motion,
			Action:  action.Label,
			History: explorer.history,
			Subject: opts.Subject,
		}, log)
		if result != nil && result.description != "" {
			description = result.description
			if inventory != nil {
				inventory.ObserveText(result.description, "gui:"+opts.Subject.Name, resultFrameIndex(frame, result))
			}
		}
		if domObserver != nil {
			if snapshot, err := domObserver.ObserveDOM(); err == nil {
				observeDOM(inventory, snapshot, "web:"+opts.Subject.Name, resultFrameIndex(frame, result))
				// The action space follows the page: after a navigation or a
				// dialog the clickable elements are different ones.
				actions = webActions(snapshot, width, height)
				explorer.markAllUntried(actions)
			}
		}
	}

	// Final idle observation, the best moment to catch flicker and churn.
	log("final idle observation")
	start := time.Now()
	time.Sleep(guiObserveSpacing * guiObserveFrames)
	finalFrames := framesInWindow(harness_.Frames(), start)
	width, height = harness_.Size()
	session.Terminal = [2]int{width, height}
	auditGUIState(ctx, cfg, auditor, session, inventory, GUIAuditInput{
		Frame:   mustCapture(harness_),
		Strip:   sampleFramesG(finalFrames, 4),
		Motion:  guiMotion(harness_, cfg, start),
		Action:  "idle observation",
		Subject: opts.Subject,
	}, log)

	// Feature audit over what the vision descriptions revealed about the app.
	if completeness, ok := completenessPersona(personas); ok {
		auditor.mu.Lock()
		auditor.SetPersona(completeness)
		outcome := auditor.AuditInventory(ctx, inventory, opts.Subject)
		auditor.mu.Unlock()
		if outcome.Error != nil {
			session.AddLog("feature audit error: %v", outcome.Error)
		}
	}

	if len(inventory.Hits) > 0 || len(inventory.Screens) > 0 {
		session.Inventory = inventory
	}
	session.Findings = auditor.Findings()
	session.Rejected = auditor.Rejected()
	session.Finalise(time.Since(harness_.Started()), len(harness_.Frames()), len(harness_.Inputs()))
	decisionCalls, visionCalls, tokens, cost := auditor.Stats()
	exploreCalls, exploreTokens, exploreCost := explorer.usage()
	session.Cost = run.Cost{
		DecisionCalls: decisionCalls + exploreCalls,
		VisionCalls:   visionCalls,
		InputTokens:   tokens + exploreTokens,
		CostUSD:       cost + exploreCost,
		Local:         cfg.Provider == "local",
	}
	session.AddLog("finished: %d findings from %d observations", len(session.Findings), len(session.Observations))
	log("done: %d findings, %d decision calls, %d vision calls", len(session.Findings), session.Cost.DecisionCalls, session.Cost.VisionCalls)
	return &GUISessionOutcome{Session: session, Frames: harness_.Frames()}, nil
}

// guiAuditOutcome carries the description forward for the next decision.
type guiAuditOutcome struct {
	result AuditResult
	// description is what the vision tier said about the window; the feature
	// inventory is built from it, so GUI subjects feed the same auditor.
	description string
}

func auditGUIState(
	ctx context.Context,
	cfg config.Config,
	auditor *Auditor,
	session *run.Session,
	inventory *audit.Inventory,
	input GUIAuditInput,
	log func(format string, args ...any),
) *guiAuditOutcome {
	if input.Frame == nil {
		return nil
	}
	outcome := auditor.AuditGUI(ctx, input)
	observation := run.Observation{
		ID:         fmt.Sprintf("obs-%d", len(session.Observations)+1),
		T:          input.Frame.T,
		GridID:     int(input.Frame.T / (100 * time.Millisecond)),
		Screen:     "gui:" + input.Subject.Name,
		Action:     input.Action,
		ModelCalls: outcome.DecisionCalls,
		Cost:       outcome.CostUSD,
		Candidates: outcome.CandidateTraces,
		Note:       fmt.Sprintf("%d candidates, %d findings", outcome.Candidates, len(outcome.Findings)),
	}
	if outcome.Error != nil {
		observation.Error = outcome.Error.Error()
		observation.Note = "audit failed"
		log("audit error: %v", outcome.Error)
	}
	session.Observations = append(session.Observations, observation)
	for _, finding := range outcome.Findings {
		log("finding [%s/%s] %s: %s", finding.SeverityName, finding.Verified, finding.Title, finding.Brief)
	}
	description := input.Description
	if description == "" {
		if answer, ok := outcome.RawAnswers["visible_defect"]; ok {
			description = fmt.Sprintf("previous audit: visible_defect=%.2f", answer)
		}
	}
	return &guiAuditOutcome{result: outcome, description: description}
}

func applyGUIAction(h harness.GUIHarness, action guiAction) error {
	switch action.Kind {
	case "key":
		return h.SendKey(action.Key)
	case "click":
		return h.Click(action.Point[0], action.Point[1])
	case "resize":
		return h.ResizeWindow(action.Size[0], action.Size[1])
	case "type":
		if err := h.Click(action.Point[0], action.Point[1]); err != nil {
			return err
		}
		return h.SendText("test value")
	case "navigate":
		if observer, ok := h.(harness.DOMObserver); ok && action.URL != "" {
			return observer.Navigate(action.URL)
		}
		return nil
	default:
		return nil
	}
}

// resultFrameIndex gives the inventory a stable id for evidence lookup.
func resultFrameIndex(frame *harness.X11Frame, _ *guiAuditOutcome) int {
	if frame == nil {
		return 0
	}
	return frame.FrameIndex()
}

func mustCapture(h harness.GUIHarness) *harness.X11Frame {
	frame, err := h.Capture()
	if err != nil {
		return nil
	}
	return frame
}

func framesInWindow(frames []*harness.X11Frame, since time.Time) []*harness.X11Frame {
	// Harness timestamps are relative to its start; convert by taking everything
	// captured after the action marker (the newest frames are the interesting
	// ones, and the ledger is short enough that this is exact in practice).
	out := []*harness.X11Frame{}
	if len(frames) == 0 {
		return out
	}
	elapsed := time.Since(since)
	cutoff := frames[len(frames)-1].T - elapsed
	for _, frame := range frames {
		if frame.T >= cutoff {
			out = append(out, frame)
		}
	}
	return out
}

func sampleFramesG(frames []*harness.X11Frame, count int) []*harness.X11Frame {
	if len(frames) <= count {
		return frames
	}
	out := make([]*harness.X11Frame, 0, count)
	for index := 0; index < count; index++ {
		out = append(out, frames[index*(len(frames)-1)/(count-1)])
	}
	return out
}

// guiExplorer picks GUI actions with the decision tier, falling back to a
// deterministic sweep when the model is unavailable.
type guiExplorer struct {
	cfg      config.Config
	set      provider.Set
	tried    map[string]bool
	groups   map[string]int
	history  []string
	calls    int
	tokens   int
	cost     float64
	fallback int
}

func newGUIExplorer(cfg config.Config, set provider.Set) *guiExplorer {
	return &guiExplorer{cfg: cfg, set: set, tried: map[string]bool{}, groups: map[string]int{}}
}

func (e *guiExplorer) usage() (int, int, float64) { return e.calls, e.tokens, e.cost }

func (e *guiExplorer) next(
	ctx context.Context,
	actions []guiAction,
	description string,
	step int,
) (guiAction, bool) {
	fresh := []guiAction{}
	for _, action := range actions {
		if !e.tried[action.ID] {
			fresh = append(fresh, action)
		}
	}
	if len(fresh) == 0 {
		return guiAction{}, false
	}
	if description == "" || e.set.Decisions == nil || e.calls >= e.cfg.Limits.MaxDecisions {
		return e.takeDeterministic(fresh), true
	}
	criteria := map[string]string{}
	for _, action := range fresh {
		criteria[action.ID] = action.Label
	}
	state := map[string]any{
		"app":     map[string]any{"name": "gui subject", "screen": "gui"},
		"history": e.history,
		"note":    description,
		"policy":  "A GUI tester opens menus, clicks controls, resizes the window and waits to catch animation problems. Prefer an action that reveals a state nobody has looked at yet.",
	}
	questions := map[string]jev.Question{
		"action": jev.Choice(
			"Which single action most likely reveals a state or a layout that has not been looked at yet?",
			criteria),
	}
	decision, err := e.set.Decisions.Decide(ctx, state, questions, 1, map[string]any{"purpose": "explore-gui"})
	if err != nil {
		return e.takeDeterministic(fresh), true
	}
	e.calls++
	e.tokens += decision.Usage.InputTokens
	e.cost += decision.Usage.CostUSD
	if answer, ok := decision.Answers["action"]; ok {
		for _, action := range fresh {
			if action.ID == answer.Choice {
				return e.take(action), true
			}
		}
	}
	return e.takeDeterministic(fresh), true
}

// markAllUntried re-arms the action space after the page changed shape.
func (e *guiExplorer) markAllUntried(actions []guiAction) {
	for _, action := range actions {
		delete(e.tried, action.ID)
	}
}

func (e *guiExplorer) takeDeterministic(fresh []guiAction) guiAction {
	if fresh[0].Group != "" {
		best := fresh[0]
		for _, action := range fresh[1:] {
			if e.groups[action.Group] < e.groups[best.Group] {
				best = action
			}
		}
		return e.take(best)
	}
	action := fresh[e.fallback%len(fresh)]
	e.fallback++
	return e.take(action)
}

func (e *guiExplorer) take(action guiAction) guiAction {
	e.tried[action.ID] = true
	e.groups[action.Group]++
	e.history = append(e.history, action.Label)
	if len(e.history) > 10 {
		e.history = e.history[len(e.history)-10:]
	}
	return action
}
