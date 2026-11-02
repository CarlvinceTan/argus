package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/harness"
	"github.com/carlvincetan/argus/internal/intent"
	"github.com/carlvincetan/argus/internal/knowledge"
	"github.com/carlvincetan/argus/internal/provider"
	"github.com/carlvincetan/argus/internal/run"
	"github.com/carlvincetan/argus/internal/screen"
	"github.com/carlvincetan/argus/internal/sensors"
)

const (
	nativeSettle  = 250 * time.Millisecond
	nativeChange  = 900 * time.Millisecond
	nativeTrack   = 4 * time.Second
	nativeFrames  = 3
	nativeSpacing = 60 * time.Millisecond
	nativeSteps   = 12
	nativeChoices = 30                     // a model cannot weigh a whole sidebar of rows
	nativeRecheck = 600 * time.Millisecond // outlasts the app's fades and slides
	nativeRest    = time.Second            // a saved state should be one the app has finished reaching
	nativeBoot    = 5 * nativeRest / 2     // a launch settles later than a step
	nativeReach   = 4 * time.Second        // how long a screen gets to reach its baseline
)

var nativeResizes = [][2]int{{640, 480}, {900, 600}, {1400, 900}}

// NativeOptions configures a run against a harness that reads the accessibility
// tree. The caller owns the harness and the app behind it.
type NativeOptions struct {
	Subject  run.Subject
	Harness  harness.GUIHarness
	MaxSteps int
	// Avoid lists label fragments the explorer must not press: controls with
	// effects outside the app, such as a system permission prompt.
	Avoid  []string
	Replay *Replay
	// Baseline makes Replay an approved baseline: the run takes its steps exactly
	// and compares every state with it. Images holds its screenshots, if any.
	Baseline bool
	Images   string
	Intents  *intent.Ledger
	Observer func(format string, args ...any)
	// Diagnostics reads anything the subject reported about itself after a run.
	Diagnostics func() []sensors.Evidence
}

// NativeOutcome is the result of a native run.
type NativeOutcome struct {
	Session  *run.Session
	Frames   []*harness.X11Frame
	Trace    *Replay
	Crashed  bool
	Replayed int
	Drifted  bool
	// Blind counts failed captures. Tree checks still ran, but nothing a model or
	// a reviewer would need to see was recorded for those steps.
	Blind int
	Perf  Perf
	// Shots holds the frame of each state: 0 at startup, then one per trace step.
	Shots map[int]*harness.X11Frame
	// Compared counts the states checked against a baseline.
	Compared int
	// Unfinished says why a baseline couldn't be followed to its end for a reason
	// that isn't the app's, which makes the run inconclusive rather than drifted.
	Unfinished string
}

// Perf holds what a run measured about speed. These are single samples on a shared
// machine, so they are numbers to compare against a baseline, never findings.
type Perf struct {
	Responses []Response `json:"responses"`
	SlowestMS int64      `json:"slowest_read_ms"` // main-thread lag proxy
	PeakRSSKB int64      `json:"peak_rss_kb"`
	GrowthKB  int64      `json:"rss_growth_kb"`
	IdleCPU   float64    `json:"idle_cpu"`
	// PixelShare is the largest image difference from a baseline, drift or not.
	PixelShare float64 `json:"pixel_share,omitempty"`
}

// Response is how long one action took to change the window.
type Response struct {
	Action string `json:"action"`
	MS     int64  `json:"ms"`
}

// nativeActions derives the action space from what the window offers.
func nativeActions(snapshot screen.AXSnapshot, avoid []string) []guiAction {
	actions := []guiAction{}
	for _, element := range snapshot.Elements {
		if element.Frame == nil || element.Frame.W < 4 || element.Frame.H < 4 || element.Disabled() ||
			sensors.IsWindowChrome(element.Subrole) || isDestructive(element.Name()) || avoided(element.Name(), avoid) {
			continue
		}
		name := element.Describe()
		switch {
		case element.Can(harness.AXPress):
			actions = append(actions, guiAction{ID: "press:" + element.Key, Kind: "press", Element: element.Key, Label: "press " + name, Group: axGroup(snapshot, element)})
		case element.Role == "AXTextField" || element.Role == "AXTextArea":
			if !element.Secure {
				for index, sample := range worstCases {
					text := sample.text
					// Pasting into a single-line field drops its line breaks, so a
					// direct set must too, or it tests a state no user can reach.
					if element.Role == "AXTextField" {
						text = strings.ReplaceAll(text, "\n", " ")
					}
					actions = append(actions, guiAction{
						ID: fmt.Sprintf("fill:%d:%s", index, element.Key), Kind: "fill", Element: element.Key, Text: text,
						Label: "fill " + name + " with " + sample.name, Group: "fill",
					})
				}
			}
		case element.Role == "AXScrollArea":
			actions = append(actions, guiAction{ID: "scroll:" + element.Key, Kind: "scroll", Element: element.Key, Label: "scroll " + name, Group: "scroll"})
		}
	}
	for _, size := range nativeResizes {
		actions = append(actions, guiAction{
			ID: fmt.Sprintf("resize:%dx%d", size[0], size[1]), Kind: "resize", Size: size, Group: "resize",
			Label: fmt.Sprintf("resize the window to %dx%d to test its layout", size[0], size[1]),
		})
	}
	return append(actions,
		guiAction{ID: "key:ESC", Kind: "key", Key: "ESC", Label: "press Escape to dismiss whatever is open", Group: "key"},
		guiAction{ID: "idle", Kind: "idle", Label: "leave it alone and watch for repainting", Group: "idle"})
}

// axGroup separates a list's rows from the controls around them.
func axGroup(snapshot screen.AXSnapshot, element screen.AXElement) string {
	// Parents always come first, so walking up always ends.
	for element.Parent != nil && *element.Parent < element.Index {
		element = snapshot.Elements[*element.Parent]
		switch element.Role {
		case "AXOutline", "AXTable", "AXList":
			return "list"
		}
	}
	return "controls"
}

func avoided(name string, avoid []string) bool {
	lower := strings.ToLower(name)
	for _, word := range avoid {
		if strings.Contains(lower, strings.ToLower(word)) {
			return true
		}
	}
	return false
}

// sceneName is a short label for a state: what is selected, and whether anything
// sits on top of the window.
func sceneName(snapshot screen.AXSnapshot) string {
	name := "nothing selected"
	sheets := 0
	for _, element := range snapshot.Elements {
		if element.Selected != nil && *element.Selected && name == "nothing selected" {
			if label := element.Name(); label != "" {
				name = label
			}
		}
		if element.Role == "AXSheet" || element.Role == "AXPopover" || element.Subrole == "AXDialog" {
			sheets++
		}
	}
	if sheets > 0 {
		name += " + overlay"
	}
	return name
}

// nativeScene summarises the window for the explorer's model call.
func nativeScene(snapshot screen.AXSnapshot) string {
	names := []string{}
	for _, element := range snapshot.Elements {
		if name := element.Name(); name != "" && len(names) < 60 {
			names = append(names, element.Describe())
		}
	}
	return "The window shows: " + strings.Join(names, ", ")
}

func applyNative(h harness.GUIHarness, observer harness.AXObserver, action guiAction) error {
	switch action.Kind {
	case "press":
		return observer.Press(action.Element)
	case "fill":
		return observer.Fill(action.Element, action.Text)
	case "scroll":
		return observer.Scroll(action.Element, "down")
	}
	return applyGUIAction(h, action)
}

// recordMeasured files evidence as findings with no model in the loop. Tree
// measurements stand on their own. Pixel motion can't tell flicker from a typing
// indicator, so without a model it stays in run.json, below the backlog.
func (a *Auditor) recordMeasured(evidence []sensors.Evidence, screenName string, frame *harness.X11Frame) {
	if frame == nil {
		return
	}
	for _, item := range evidence {
		class := knowledge.ClassFromCheck(item.Check)
		verified, probability := "deterministic", item.Confidence
		if !strings.HasPrefix(item.Check, "structure") {
			verified, probability = "unverified", minFloat(item.Confidence, a.cfg.Thresholds.VerifyLow)
		}
		finding := run.Finding{
			Title:         guiCheckName(item.Check),
			Brief:         item.Detail,
			Group:         screenName,
			Screen:        screenName,
			Feature:       item.Snippet,
			Region:        item.Region,
			Check:         item.Check,
			Class:         class,
			Severity:      run.FromScore(item.Severity),
			SeverityScore: item.Severity,
			DefectProb:    probability,
			Confidence:    item.Confidence,
			Detail:        item.Detail,
			Measured:      item.Measured,
			Events:        1,
			FirstSeen:     frame.T,
			LastSeen:      frame.T,
			GridID:        frame.FrameIndex(),
			Verified:      verified,
			Tags:          []string{"native", item.Check},
		}
		a.calibrate(&finding, item.Check, class)
		a.merge(finding)
	}
}

// nativeRun carries one run's state between its steps.
type nativeRun struct {
	ctx       context.Context
	cfg       config.Config
	opts      NativeOptions
	h         harness.GUIHarness
	observer  harness.AXObserver
	auditor   *Auditor
	explorer  *guiExplorer
	session   *run.Session
	kept      []*harness.X11Frame
	blind     int
	compared  int
	confirmed map[string]bool
	perf      Perf
	baseRSS   int64
	screen    string
	log       func(format string, args ...any)
}

// measure runs the tree checks and drops whatever a second look no longer finds,
// so transitions aren't reported. Evidence already confirmed needs no second look.
func (r *nativeRun) measure(snapshot screen.AXSnapshot) []sensors.Evidence {
	scale := r.observer.Scale()
	found := sensors.RunAXChecks(snapshot, scale)
	settled := true
	for _, item := range found {
		settled = settled && r.confirmed[sensors.EvidenceKey(item)]
	}
	if settled {
		return found
	}
	time.Sleep(nativeRecheck)
	later, err := r.observer.ObserveAX()
	if err != nil {
		return found
	}
	found = sensors.Persisting(found, sensors.RunAXChecks(later, scale))
	for _, item := range found {
		r.confirmed[sensors.EvidenceKey(item)] = true
	}
	return found
}

// audit measures one state and, when a model is configured, has it judge what
// was measured. A screen unchanged from the replayed run costs no model call.
func (r *nativeRun) audit(
	snapshot screen.AXSnapshot,
	frame *harness.X11Frame,
	since time.Time,
	action string,
	extra []sensors.Evidence,
	unchanged bool,
) {
	evidence := append(r.measure(snapshot), extra...)
	motion := sensors.MotionReportGUI{}
	if frame != nil {
		evidence = keepVisible(evidence, frame)
		r.kept = append(r.kept, frame)
		motion = guiMotion(r.h, r.cfg, since)
	} else {
		frame = &harness.X11Frame{T: time.Since(r.h.Started())}
	}
	motion.Evidence = append(motion.Evidence, evidence...)
	if r.auditor.providers.Decisions == nil || unchanged || frame.Image == nil {
		r.auditor.recordMeasured(motion.Evidence, r.screen, frame)
		r.session.Observations = append(r.session.Observations, run.Observation{
			ID: fmt.Sprintf("obs-%d", len(r.session.Observations)+1), T: frame.T,
			GridID: frame.FrameIndex(), Screen: r.screen, Action: action,
			Note: fmt.Sprintf("%d measurements, no model call", len(motion.Evidence)),
		})
		return
	}
	outcome := auditGUIState(r.ctx, r.cfg, r.auditor, r.session, nil, GUIAuditInput{
		Frame: frame, Strip: sampleFramesG(framesInWindow(r.h.Frames(), since), 3),
		Motion: motion, Action: action, History: r.explorer.history, Subject: r.opts.Subject,
		Description: nativeScene(snapshot),
	}, r.log)
	if outcome == nil || outcome.result.Error != nil {
		// A model failure must not cost a measurement.
		r.auditor.recordMeasured(motion.Evidence, r.screen, frame)
	}
}

// drift compares a state with its baseline, giving the screen time to reach it: a
// slow fetch or a splash isn't drift, and only a difference that stays is. It returns
// the state it finished on. Pixels only speak when the structure agrees.
func (r *nativeRun) drift(
	want *sensors.State,
	snapshot screen.AXSnapshot,
	frame *harness.X11Frame,
	index int,
	step string,
) ([]sensors.Evidence, screen.AXSnapshot, *harness.X11Frame) {
	if !r.opts.Baseline || want == nil {
		return nil, snapshot, frame
	}
	r.compared++
	scale := r.observer.Scale()
	image, err := harness.LoadPNG(filepath.Join(r.opts.Images, fmt.Sprintf("%d.png", index)))
	if err != nil {
		image = nil
	}
	masks := sensors.Masks(*want, scale, func(name string) bool {
		_, known := r.opts.Intents.Match(sensors.DriftGone, name, r.opts.Subject.Name)
		return known
	})
	compare := func(snapshot screen.AXSnapshot, frame *harness.X11Frame) []sensors.Evidence {
		found := sensors.Drift(*want, sensors.StateOf(snapshot), scale, step)
		if len(found) > 0 || image == nil || frame == nil || frame.Image == nil {
			return found
		}
		share, area := sensors.PixelDrift(image, frame.Image, masks)
		r.perf.PixelShare = max(r.perf.PixelShare, share)
		return sensors.PixelEvidence(share, area, step)
	}
	found := compare(snapshot, frame)
	for deadline := time.Now().Add(nativeReach); len(found) > 0 && time.Now().Before(deadline); {
		time.Sleep(nativeRecheck / 2)
		later, err := r.observer.ObserveAX()
		if err != nil {
			break
		}
		snapshot = later
		if shot := mustCapture(r.h); shot != nil {
			frame = shot
		}
		found = compare(snapshot, frame)
	}
	return found, snapshot, frame
}

// keepVisible drops a blank-window reading that the pixels contradict: a splash or
// a canvas fills a window while exposing nothing to accessibility.
func keepVisible(evidence []sensors.Evidence, frame *harness.X11Frame) []sensors.Evidence {
	if frame.Image == nil || !sensors.ShowsContent(frame.Image) {
		return evidence
	}
	out := make([]sensors.Evidence, 0, len(evidence))
	for _, item := range evidence {
		if item.Check != sensors.AXBlank {
			out = append(out, item)
		}
	}
	return out
}

// settled reads the window until it stops changing, and measures the way there.
// dispatch is how long the action itself took: a press waits on the app's main
// thread, so a slow handler spends its time there, before any read can start.
func (r *nativeRun) settled(before screen.AXSnapshot, action guiAction, dispatch time.Duration) (screen.AXSnapshot, []sensors.Evidence, error) {
	// A resize goes through another tool that verifies itself, so its time isn't the app's.
	timed := action.Kind != "resize"
	if ms := dispatch.Milliseconds(); timed && ms > r.perf.SlowestMS {
		r.perf.SlowestMS = ms
	}
	readings := r.observer.Track(nativeSettle, nativeChange, nativeTrack)
	if len(readings) == 0 {
		snapshot, err := r.observer.ObserveAX()
		return snapshot, nil, err
	}
	for _, reading := range readings {
		if took := reading.Took.Milliseconds(); timed && took > r.perf.SlowestMS {
			r.perf.SlowestMS = took
		}
	}
	for _, reading := range readings {
		if timed && reading.Snapshot.Signature() != before.Signature() {
			r.perf.Responses = append(r.perf.Responses, Response{Action: action.Label, MS: (dispatch + reading.At).Milliseconds()})
			break
		}
	}
	final := readings[len(readings)-1].Snapshot
	scale := r.observer.Scale()
	evidence := sensors.Flashes(before, readings, scale)
	if action.Kind == "press" {
		if element, ok := before.Find(action.Element); ok {
			evidence = append(evidence, sensors.Shifts(before, final, element, scale)...)
		}
	}
	return final, evidence, nil
}

func (r *nativeRun) sample() {
	_, rss, err := r.observer.Usage()
	if err != nil {
		return
	}
	if r.baseRSS == 0 {
		r.baseRSS = rss
	}
	if rss > r.perf.PeakRSSKB {
		r.perf.PeakRSSKB = rss
	}
	r.perf.GrowthKB = rss - r.baseRSS
}

func (r *nativeRun) capture() *harness.X11Frame {
	r.h.Observe(nativeFrames-1, nativeSpacing)
	frame, err := r.h.Capture()
	if err != nil {
		if r.blind == 0 {
			r.session.AddNote("capture failed: " + err.Error())
		}
		r.blind++
		return nil
	}
	return frame
}

// inert reports a press that did nothing. Pressing what is already selected rightly
// changes nothing, a slow app isn't a dead control, and a native press can be
// dropped, so a control is only inert once a second press has failed too.
// recovered means that second press worked and the screen has moved on.
func (r *nativeRun) inert(
	action guiAction,
	before, after screen.AXSnapshot,
	beforeFrame, afterFrame *harness.X11Frame,
) (found []sensors.Evidence, recovered bool) {
	if action.Kind != "press" || beforeFrame == nil || afterFrame == nil ||
		afterFrame.Digest != beforeFrame.Digest || after.Signature() != before.Signature() {
		return nil, false
	}
	element, ok := before.Find(action.Element)
	if !ok || before.Selected(element) {
		return nil, false
	}
	same := func() bool {
		later, err := r.observer.ObserveAX()
		frame := mustCapture(r.h)
		return err == nil && frame != nil && frame.Digest == beforeFrame.Digest && later.Signature() == before.Signature()
	}
	time.Sleep(2 * nativeRecheck)
	if !same() {
		return nil, true
	}
	if err := r.observer.Press(action.Element); err != nil {
		return nil, false
	}
	r.h.WaitForChange(nativeChange)
	time.Sleep(2 * nativeRecheck)
	if !same() {
		r.session.AddNote(fmt.Sprintf("%s needed a second press: the first one did nothing", element.Describe()))
		return nil, true
	}
	return []sensors.Evidence{sensors.InertControl(before, element, r.observer.Scale())}, false
}

// RunNativeSession explores a native app through its accessibility tree.
func RunNativeSession(
	ctx context.Context,
	cfg config.Config,
	providers provider.Set,
	opts NativeOptions,
) (*NativeOutcome, error) {
	observer, ok := opts.Harness.(harness.AXObserver)
	if !ok {
		return nil, fmt.Errorf("harness %s cannot read an accessibility tree", opts.Harness.Name())
	}
	log := func(format string, args ...any) {
		if opts.Observer != nil {
			opts.Observer(format, args...)
		}
	}
	maxSteps := opts.MaxSteps
	if maxSteps == 0 {
		maxSteps = nativeSteps
	}
	h := opts.Harness
	session := run.NewSession(opts.Subject, providers.Manifest, cfg.Provider, 0, 0)
	session.Kind = run.KindGUI
	session.AddNote("native app driven through its accessibility tree (" + h.Name() + ")")
	auditor := NewAuditor(cfg, providers)
	auditor.SetKind(run.KindGUI)
	auditor.SetIntents(opts.Intents, opts.Subject.Name)
	state := &nativeRun{
		ctx: ctx, cfg: cfg, opts: opts, h: h, observer: observer, auditor: auditor,
		explorer: newGUIExplorer(cfg, providers), session: session, confirmed: map[string]bool{},
		screen: "native:" + opts.Subject.Name, log: log,
	}
	outcome := &NativeOutcome{Trace: &Replay{}}
	state.sample()

	h.WaitIdle(nativeSettle, 3*time.Second)
	snapshot, err := observer.ObserveAX()
	if err != nil {
		return nil, err
	}
	time.Sleep(nativeBoot)
	if snapshot, err = observer.ObserveAX(); err != nil {
		return nil, err
	}
	since := time.Now()
	frame := state.capture()
	var startState *sensors.State
	if opts.Baseline {
		startState = opts.Replay.State
		maxSteps = len(opts.Replay.Steps)
	}
	var startup []sensors.Evidence
	startup, snapshot, frame = state.drift(startState, snapshot, frame, 0, "startup")
	state.audit(snapshot, frame, since, "startup", startup, false)
	observed := sensors.StateOf(snapshot)
	outcome.Trace.Start, outcome.Trace.Scene, outcome.Trace.State = snapshot.Signature(), sceneName(snapshot), &observed
	shots := map[int]*harness.X11Frame{0: frame}
	width, height := h.Size()
	session.Terminal = [2]int{width, height}

	replaying := opts.Replay != nil
	for step := 1; step <= maxSteps; step++ {
		if ctx.Err() != nil {
			session.AddNote("run stopped early: " + ctx.Err().Error())
			break
		}
		actions := nativeActions(snapshot, opts.Avoid)
		var action guiAction
		var planned *ReplayStep
		if replaying && step <= len(opts.Replay.Steps) {
			wanted := opts.Replay.Steps[step-1]
			for _, candidate := range actions {
				if candidate.ID == wanted.Action.ID {
					action, planned = state.explorer.take(candidate), &wanted
					break
				}
			}
			if planned == nil {
				replaying, outcome.Drifted = false, true
				if opts.Baseline {
					// Nothing past here can be compared, and exploring would only add noise.
					session.AddNote(fmt.Sprintf("baseline stopped at step %d: %q is gone", step, wanted.Action.Label))
					state.audit(snapshot, frame, time.Now(), "baseline stopped", []sensors.Evidence{sensors.MissingStep(wanted.Action.Label, step)}, true)
					break
				}
				session.AddNote(fmt.Sprintf("replay drifted at step %d: %q is gone; exploring from here", step, wanted.Action.Label))
			}
		} else if replaying {
			replaying = false
		}
		if planned == nil {
			if len(actions) > nativeChoices {
				actions = actions[len(actions)-nativeChoices:]
			}
			picked, ok := state.explorer.next(ctx, actions, nativeScene(snapshot), step)
			if !ok {
				session.AddNote("exploration stopped: no action left to try")
				break
			}
			action = picked
		}
		log("step %d/%d: %s", step, maxSteps, action.Label)
		before, beforeFrame := snapshot, frame
		since = time.Now()
		err = applyNative(h, observer, action)
		dispatch := time.Since(since)
		if err != nil && opts.Baseline && observer.Alive() {
			time.Sleep(time.Second)
			err = applyNative(h, observer, action)
		}
		if err != nil {
			if !observer.Alive() {
				outcome.Crashed = true
				break
			}
			session.AddNote(fmt.Sprintf("action %q failed: %v", action.Label, err))
			if opts.Baseline {
				outcome.Unfinished = fmt.Sprintf("baseline step %d couldn't run: %v", step, err)
				break
			}
			continue
		}
		var measured []sensors.Evidence
		snapshot, measured, err = state.settled(before, action, dispatch)
		if !observer.Alive() {
			outcome.Crashed = true
			break
		}
		if err != nil {
			session.AddNote("observation failed: " + err.Error())
			break
		}
		time.Sleep(nativeRest)
		if snapshot, err = observer.ObserveAX(); err != nil {
			session.AddNote("observation failed: " + err.Error())
			break
		}
		state.sample()
		frame = state.capture()
		// Every screen needs a way out once it changes.
		if snapshot.Signature() != before.Signature() {
			delete(state.explorer.tried, "key:ESC")
		}
		extra, recovered := state.inert(action, before, snapshot, beforeFrame, frame)
		if recovered {
			if snapshot, err = observer.ObserveAX(); err != nil {
				session.AddNote("observation failed: " + err.Error())
				break
			}
			frame = state.capture()
		}
		extra = append(extra, measured...)
		unchanged := false
		if planned != nil {
			outcome.Replayed++
			unchanged = frame != nil && planned.Signature == snapshot.Signature() && planned.Digest == frame.Digest
			if planned.Signature != snapshot.Signature() {
				outcome.Drifted = true
			}
			var drifted []sensors.Evidence
			drifted, snapshot, frame = state.drift(planned.State, snapshot, frame, len(outcome.Trace.Steps)+1, action.Label)
			extra = append(extra, drifted...)
		}
		state.audit(snapshot, frame, since, action.Label, extra, unchanged)
		digest := ""
		if frame != nil {
			digest = frame.Digest
		}
		observed := sensors.StateOf(snapshot)
		outcome.Trace.Steps = append(outcome.Trace.Steps, ReplayStep{
			Action: action, Signature: snapshot.Signature(), Digest: digest, Scene: sceneName(snapshot), State: &observed,
		})
		shots[len(outcome.Trace.Steps)] = frame
	}

	if outcome.Crashed {
		state.crashed(snapshot, frame)
	} else {
		log("final idle observation")
		since = time.Now()
		h.Observe(6, nativeSpacing)
		state.audit(snapshot, state.capture(), since, "idle observation", nil, false)
		if cpu, _, err := observer.Usage(); err == nil {
			state.perf.IdleCPU = cpu
		}
	}
	if opts.Diagnostics != nil {
		auditor.recordMeasured(opts.Diagnostics(), state.screen, &harness.X11Frame{T: time.Since(h.Started())})
	}

	session.Findings = auditor.Findings()
	session.Rejected = auditor.Rejected()
	session.Finalise(time.Since(h.Started()), len(state.kept), len(h.Inputs()))
	decisionCalls, visionCalls, tokens, cost := auditor.Stats()
	exploreCalls, exploreTokens, exploreCost := state.explorer.usage()
	session.Cost = run.Cost{
		DecisionCalls: decisionCalls + exploreCalls,
		VisionCalls:   visionCalls,
		InputTokens:   tokens + exploreTokens,
		CostUSD:       cost + exploreCost,
		Local:         cfg.Provider == "local",
	}
	log("done: %d findings, %d decision calls, %d vision calls", len(session.Findings), session.Cost.DecisionCalls, session.Cost.VisionCalls)
	outcome.Session, outcome.Frames, outcome.Blind, outcome.Perf = session, state.kept, state.blind, state.perf
	outcome.Shots, outcome.Compared = shots, state.compared
	return outcome, nil
}

// crashed reports the subject dying, the one finding that needs no judgement.
func (r *nativeRun) crashed(snapshot screen.AXSnapshot, frame *harness.X11Frame) {
	if frame == nil {
		frame = &harness.X11Frame{T: time.Since(r.h.Started())}
	}
	// A dying process can outlive the next action, so name the recent ones.
	last := "startup"
	if count := len(r.explorer.history); count > 0 {
		last = strings.Join(r.explorer.history[max(0, count-2):], ", then ")
	}
	r.auditor.recordMeasured([]sensors.Evidence{{
		Check:      sensors.AXCrash,
		Region:     snapshot.Region(snapshot.Window, r.observer.Scale()),
		Detail:     "the app exited during: " + last,
		Snippet:    "the app exited",
		Severity:   4,
		Confidence: 1,
	}}, r.screen, frame)
}
