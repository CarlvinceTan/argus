package agent

import (
	"context"
	"errors"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/harness"
	"github.com/carlvincetan/argus/internal/intent"
	"github.com/carlvincetan/argus/internal/jev"
	"github.com/carlvincetan/argus/internal/provider"
	"github.com/carlvincetan/argus/internal/run"
	"github.com/carlvincetan/argus/internal/screen"
	"github.com/carlvincetan/argus/internal/sensors"
)

// fakeApp scripts a window: Open reveals a control, and crash names one that kills it.
type fakeApp struct {
	started time.Time
	opened  bool
	dead    bool
	crash   string
	removed string
	extra   *screen.AXElement
	// extraReads limits extra to its first reads, then it is gone.
	extraReads int
	shots      int
	blind      bool
	reads      int
	// dropOpen ignores this many presses of Open, as a busy app can.
	dropOpen int
	presses  []string
}

func fakeElement(index int, role, label string, x float64, actions ...string) screen.AXElement {
	root := 0
	return screen.AXElement{Index: index, Parent: &root, Role: role, Label: label,
		Frame: &screen.AXFrame{X: x, Y: 40, W: 90, H: 30}, Actions: actions}
}

func (f *fakeApp) ObserveAX() (screen.AXSnapshot, error) {
	if f.dead {
		return screen.AXSnapshot{}, errors.New("session ended")
	}
	elements := []screen.AXElement{{Index: 0, Role: "AXWindow", Label: "App", Frame: &screen.AXFrame{W: 800, H: 600}}}
	for _, label := range []string{"Open", "Quiet", "Boom"} {
		if label != f.removed {
			elements = append(elements, fakeElement(len(elements), "AXButton", label, float64(len(elements))*100, harness.AXPress))
		}
	}
	if f.opened {
		elements = append(elements, fakeElement(len(elements), "AXButton", "Inside", 500, harness.AXPress))
	}
	if f.extra != nil && (f.extraReads == 0 || f.reads < f.extraReads) {
		extra := *f.extra
		extra.Index = len(elements)
		elements = append(elements, extra)
	}
	f.reads++
	snapshot := screen.AXSnapshot{Elements: elements, Complete: true, Window: screen.AXFrame{W: 800, H: 600}}
	snapshot.AssignKeys()
	return snapshot, nil
}

func (f *fakeApp) Press(key string) error {
	f.presses = append(f.presses, key)
	switch {
	case f.crash != "" && strings.Contains(key, f.crash):
		f.dead = true
		return errors.New("session ended")
	case strings.Contains(key, "Open"):
		if f.dropOpen > 0 {
			f.dropOpen--
			return nil
		}
		f.opened = true
	}
	return nil
}

func (f *fakeApp) Fill(string, string) error   { return nil }
func (f *fakeApp) Scroll(string, string) error { return nil }
func (f *fakeApp) Scale() float64              { return 1 }
func (f *fakeApp) Alive() bool                 { return !f.dead }
func (f *fakeApp) Name() string                { return "fake" }
func (f *fakeApp) Started() time.Time          { return f.started }
func (f *fakeApp) Size() (int, int)            { return 800, 600 }
func (f *fakeApp) SendKey(string) error        { return nil }
func (f *fakeApp) SendText(string) error       { return nil }
func (f *fakeApp) Click(int, int) error        { return nil }
func (f *fakeApp) ResizeWindow(int, int) error { return nil }
func (f *fakeApp) Frames() []*harness.X11Frame { return nil }
func (f *fakeApp) Inputs() []harness.InputEvent {
	return nil
}
func (f *fakeApp) WaitForChange(time.Duration) bool { return false }
func (f *fakeApp) WaitIdle(time.Duration, time.Duration) bool {
	return true
}
func (f *fakeApp) Close() error { return nil }

func (f *fakeApp) Track(time.Duration, time.Duration, time.Duration) []screen.AXReading {
	snapshot, err := f.ObserveAX()
	if err != nil {
		return nil
	}
	return []screen.AXReading{{Snapshot: snapshot, At: time.Millisecond}}
}

func (f *fakeApp) Usage() (float64, int64, error) { return 1, 1024, nil }

func (f *fakeApp) Capture() (*harness.X11Frame, error) {
	if f.blind {
		return nil, errors.New("no window to capture")
	}
	f.shots++
	digest := "closed"
	if f.opened {
		digest = "opened"
	}
	return &harness.X11Frame{T: time.Duration(f.shots) * time.Second, Digest: digest, Width: 8, Height: 6,
		Image: image.NewRGBA(image.Rect(0, 0, 8, 6)), TotalPixels: 48}, nil
}

func (f *fakeApp) Observe(count int, _ time.Duration) []*harness.X11Frame {
	frames := []*harness.X11Frame{}
	for index := 0; index < count; index++ {
		frame, _ := f.Capture()
		frames = append(frames, frame)
	}
	return frames
}

func nativeConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.CalibrationPath = filepath.Join(t.TempDir(), "none.json")
	return cfg
}

func runFake(t *testing.T, app *fakeApp, steps int, replay *Replay) *NativeOutcome {
	t.Helper()
	app.started = time.Now()
	outcome, err := RunNativeSession(context.Background(), nativeConfig(t), provider.Set{}, NativeOptions{
		Subject: run.Subject{Name: "fake"}, Harness: app, MaxSteps: steps, Avoid: []string{"avoided"}, Replay: replay,
	})
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func TestNativeActionsSkipWhatMustNotBePressed(t *testing.T) {
	root := 0
	offLabel := false
	elements := []screen.AXElement{
		{Index: 0, Role: "AXWindow", Label: "App"},
		fakeElement(1, "AXButton", "Send", 10, harness.AXPress),
		fakeElement(2, "AXButton", "Delete chat", 110, harness.AXPress),
		fakeElement(3, "AXButton", "Start avoided thing", 210, harness.AXPress),
		{Index: 4, Parent: &root, Role: "AXButton", Subrole: "AXCloseButton", Label: "Close",
			Frame: &screen.AXFrame{W: 16, H: 16}, Actions: []string{harness.AXPress}},
		{Index: 5, Parent: &root, Role: "AXButton", Label: "Off", Enabled: &offLabel,
			Frame: &screen.AXFrame{W: 40, H: 40}, Actions: []string{harness.AXPress}},
		fakeElement(6, "AXTextField", "Message", 310),
		{Index: 7, Parent: &root, Role: "AXTextField", Label: "Password", Secure: true, Frame: &screen.AXFrame{W: 40, H: 40}},
	}
	snapshot := screen.AXSnapshot{Elements: elements}
	snapshot.AssignKeys()
	ids := []string{}
	for _, action := range nativeActions(snapshot, []string{"avoided"}) {
		ids = append(ids, action.ID)
	}
	got := strings.Join(ids, " ")
	for _, wanted := range []string{"press:AXButton|Send#0", "fill:0:AXTextField|Message#0"} {
		if !strings.Contains(got, wanted) {
			t.Fatalf("missing %s in %s", wanted, got)
		}
	}
	for _, refused := range []string{"Delete", "avoided", "Close", "Off", "Password"} {
		if strings.Contains(got, refused) {
			t.Fatalf("offered %s in %s", refused, got)
		}
	}
}

func TestNativeRunFindsNewControlsAndRecordsATrace(t *testing.T) {
	app := &fakeApp{}
	outcome := runFake(t, app, 6, nil)
	if outcome.Crashed || len(outcome.Trace.Steps) == 0 {
		t.Fatalf("outcome = %+v", outcome)
	}
	if !strings.Contains(strings.Join(app.presses, " "), "Open") {
		t.Fatalf("presses = %v", app.presses)
	}
}

func TestNativeRunReportsAnInertControl(t *testing.T) {
	outcome := runFake(t, &fakeApp{removed: "Boom"}, 2, &Replay{Steps: []ReplayStep{
		{Action: guiAction{ID: "press:AXButton|Quiet#0", Kind: "press", Element: "AXButton|Quiet#0", Label: "press Quiet"}},
	}})
	found := false
	for _, finding := range outcome.Session.Findings {
		found = found || finding.Check == sensors.AXInert
	}
	if !found {
		t.Fatalf("findings = %+v", outcome.Session.Findings)
	}
}

func TestNativeRunReportsACrash(t *testing.T) {
	outcome := runFake(t, &fakeApp{crash: "Boom"}, 1, &Replay{Steps: []ReplayStep{
		{Action: guiAction{ID: "press:AXButton|Boom#0", Kind: "press", Element: "AXButton|Boom#0", Label: "press Boom"}},
	}})
	if !outcome.Crashed || len(outcome.Session.Findings) != 1 || outcome.Session.Findings[0].Check != sensors.AXCrash {
		t.Fatalf("outcome = %+v, findings = %+v", outcome, outcome.Session.Findings)
	}
	if !outcome.Session.Findings[0].Reported {
		t.Fatal("a crash must always reach the backlog")
	}
}

func TestReplayRepeatsTheTraceAndDriftsToExploring(t *testing.T) {
	first := runFake(t, &fakeApp{}, 4, nil)
	if err := first.Trace.Save(filepath.Join(t.TempDir(), "x", "trace.json")); err != nil {
		t.Fatal(err)
	}
	app := &fakeApp{}
	second := runFake(t, app, len(first.Trace.Steps), first.Trace)
	if second.Replayed != len(first.Trace.Steps) || second.Drifted {
		t.Fatalf("replayed %d of %d, drifted %t", second.Replayed, len(first.Trace.Steps), second.Drifted)
	}
	script := &Replay{Steps: []ReplayStep{
		{Action: guiAction{ID: "press:AXButton|Gone#0", Kind: "press", Element: "AXButton|Gone#0", Label: "press Gone"}},
	}}
	drifted := runFake(t, &fakeApp{}, 1, script)
	if !drifted.Drifted || drifted.Replayed != 0 || len(drifted.Trace.Steps) != 1 {
		t.Fatalf("drift outcome = %+v", drifted)
	}
}

// A failed screenshot must never read as a clean run.
func TestNativeRunKeepsMeasuringWithoutScreenshots(t *testing.T) {
	app := &fakeApp{blind: true, removed: "Open"}
	root := 0
	app.extra = &screen.AXElement{Parent: &root, Role: "AXButton", Frame: &screen.AXFrame{X: 700, Y: 40, W: 90, H: 30}, Actions: []string{harness.AXPress}}
	outcome := runFake(t, app, 1, nil)
	if outcome.Blind == 0 {
		t.Fatal("failed captures were not counted")
	}
	found := false
	for _, finding := range outcome.Session.Findings {
		found = found || finding.Check == sensors.AXUnlabelled
	}
	if !found {
		t.Fatalf("tree checks stopped without a screenshot: %+v", outcome.Session.Findings)
	}
	if !strings.Contains(strings.Join(outcome.Session.Notes, " "), "capture failed") {
		t.Fatalf("notes = %v", outcome.Session.Notes)
	}
}

func fakeLedger(t *testing.T, check, element string) (string, *intent.Ledger) {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "e2e"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "View.swift"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	ledger, err := intent.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Add(intent.Intent{ID: "known", Check: check, Element: element, Reason: "on purpose", Paths: []string{"View.swift"}}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Save(); err != nil {
		t.Fatal(err)
	}
	return repo, ledger
}

func TestIntentsSilenceAFindingOnlyWhileTheirSourcesHold(t *testing.T) {
	repo, ledger := fakeLedger(t, sensors.AXClipped, `Button "one"`)
	finding := func(ledger *intent.Ledger) run.Finding {
		auditor := NewAuditor(nativeConfig(t), provider.Set{})
		auditor.SetKind(run.KindGUI)
		auditor.SetIntents(ledger, "chat")
		auditor.recordMeasured([]sensors.Evidence{
			{Check: sensors.AXClipped, Severity: 2.5, Confidence: 0.9, Snippet: `Button "one"`, Region: screen.Region{X0: 50, X1: 60}},
		}, "native:chat", &harness.X11Frame{T: time.Second})
		return auditor.Findings()[0]
	}
	quiet := finding(ledger)
	if quiet.Reported || !quiet.Intentional {
		t.Fatalf("a confirmed intent should keep this out of the backlog: %+v", quiet)
	}
	if err := os.WriteFile(filepath.Join(repo, "View.swift"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded, err := intent.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	loud := finding(reloaded)
	if !loud.Reported || loud.Intentional || !strings.Contains(strings.Join(loud.Tags, " "), run.TagStaleIntent+"known") {
		t.Fatalf("a stale intent must not suppress: %+v", loud)
	}
}

func TestResizedElementStaysOneFinding(t *testing.T) {
	auditor := NewAuditor(nativeConfig(t), provider.Set{})
	auditor.SetKind(run.KindGUI)
	frame := &harness.X11Frame{T: time.Second}
	for _, x := range []int{100, 500, 900} {
		auditor.recordMeasured([]sensors.Evidence{
			{Check: sensors.AXClipped, Severity: 2.5, Confidence: 0.9, Snippet: `Button "one"`, Region: screen.Region{X0: x, X1: x + 10}},
		}, "native:chat", frame)
	}
	if got := auditor.Findings(); len(got) != 1 || got[0].Events != 3 {
		t.Fatalf("findings = %+v", got)
	}
}

// A banner fading out is measured once, but a second look no longer finds it.
func TestTransientEvidenceIsDropped(t *testing.T) {
	app := &fakeApp{removed: "Boom"}
	root := 0
	app.extra = &screen.AXElement{Parent: &root, Role: "AXButton", Frame: &screen.AXFrame{X: 700, Y: 40, W: 90, H: 30}, Actions: []string{harness.AXPress}}
	app.extraReads = 1
	outcome := runFake(t, app, 1, nil)
	for _, finding := range outcome.Session.Findings {
		if finding.Check == sensors.AXUnlabelled {
			t.Fatalf("a transient control was reported: %+v", finding)
		}
	}
}

func TestADroppedPressIsNotAnInertControl(t *testing.T) {
	app := &fakeApp{dropOpen: 1}
	outcome := runFake(t, app, 1, &Replay{Steps: []ReplayStep{
		{Action: guiAction{ID: "press:AXButton|Open#0", Kind: "press", Element: "AXButton|Open#0", Label: "press Open"}},
	}})
	for _, finding := range outcome.Session.Findings {
		if finding.Check == sensors.AXInert {
			t.Fatalf("a dropped press was reported as a dead control: %+v", finding)
		}
	}
	if !strings.Contains(strings.Join(outcome.Session.Notes, " "), "second press") {
		t.Fatalf("notes = %v", outcome.Session.Notes)
	}
}

func TestPressingWhatIsSelectedIsNotInert(t *testing.T) {
	yes := true
	root := 0
	row := 1
	snapshot := screen.AXSnapshot{Elements: []screen.AXElement{
		{Index: 0, Role: "AXWindow"},
		{Index: 1, Parent: &root, Role: "AXRow", Selected: &yes},
		{Index: 2, Parent: &row, Role: "AXButton", Label: "Chat"},
		{Index: 3, Parent: &root, Role: "AXButton", Label: "Other"},
	}}
	if !snapshot.Selected(snapshot.Elements[2]) || snapshot.Selected(snapshot.Elements[3]) {
		t.Fatal("selection should follow the row and stop there")
	}
}

func TestMotionNeedsAModelToReachTheBacklog(t *testing.T) {
	auditor := NewAuditor(nativeConfig(t), provider.Set{})
	auditor.SetKind(run.KindGUI)
	frame := &harness.X11Frame{T: time.Second}
	auditor.recordMeasured([]sensors.Evidence{
		{Check: sensors.GUIFlicker, Severity: 3, Confidence: 0.95, Snippet: "dots"},
		{Check: sensors.AXClipped, Severity: 2.5, Confidence: 0.9, Snippet: "button", Region: screen.Region{X0: 50, X1: 60}},
	}, "native:test", frame)
	reported := map[string]bool{}
	for _, finding := range auditor.Findings() {
		reported[finding.Check] = finding.Reported
	}
	if reported[sensors.GUIFlicker] || !reported[sensors.AXClipped] {
		t.Fatalf("reported = %v", reported)
	}
}

// countingModel answers nothing and counts what it was asked.
type countingModel struct{ calls int }

func (m *countingModel) Name() string                      { return "counting" }
func (m *countingModel) Calls() int                        { return m.calls }
func (m *countingModel) CostUSD() float64                  { return 0 }
func (m *countingModel) Transcript() []jev.TranscriptEntry { return nil }
func (m *countingModel) Decide(context.Context, any, map[string]jev.Question, int, map[string]any) (jev.Result, error) {
	m.calls++
	return jev.Result{Answers: map[string]jev.Answer{}}, nil
}

func TestReplaySpendsNoModelCallsOnAnUnchangedApp(t *testing.T) {
	run_ := func(model *countingModel, app *fakeApp, replay *Replay) *NativeOutcome {
		app.started = time.Now()
		outcome, err := RunNativeSession(context.Background(), nativeConfig(t), provider.Set{Decisions: model}, NativeOptions{
			Subject: run.Subject{Name: "fake"}, Harness: app, MaxSteps: 4, Replay: replay,
		})
		if err != nil {
			t.Fatal(err)
		}
		return outcome
	}
	explored, replayed := &countingModel{}, &countingModel{}
	first := run_(explored, &fakeApp{}, nil)
	second := run_(replayed, &fakeApp{}, first.Trace)
	if second.Replayed != len(first.Trace.Steps) || second.Drifted {
		t.Fatalf("replayed %d of %d, drifted %t", second.Replayed, len(first.Trace.Steps), second.Drifted)
	}
	// Only the first and last screens, which no trace covers, reach the model.
	if replayed.calls != 2 || explored.calls <= replayed.calls {
		t.Fatalf("model calls: explored %d, replayed %d", explored.calls, replayed.calls)
	}
}

func TestStateGraphDrawsStatesOnceAndSkipsLoops(t *testing.T) {
	trace := &Replay{Start: "a", Scene: "Chief: home", Steps: []ReplayStep{
		{Action: guiAction{ID: "x", Label: `press "Open"`}, Signature: "b", Scene: "Chief + overlay"},
		{Action: guiAction{ID: "y", Label: "idle"}, Signature: "b", Scene: "Chief + overlay"},
		{Action: guiAction{ID: "z", Label: "press Escape"}, Signature: "a", Scene: "Chief: home"},
	}}
	graph := trace.Mermaid()
	for _, wanted := range []string{"stateDiagram-v2", "[*] --> s0", "s0 --> s1: press Open", "s1 --> s0: press Escape", "s0: Chief  home"} {
		if !strings.Contains(graph, wanted) {
			t.Fatalf("graph is missing %q:\n%s", wanted, graph)
		}
	}
	if strings.Contains(graph, "idle") {
		t.Fatalf("a step that stayed put was drawn:\n%s", graph)
	}
}

func TestWorstCasesAreOfferedForEveryField(t *testing.T) {
	root := 0
	snapshot := screen.AXSnapshot{Elements: []screen.AXElement{
		{Index: 0, Role: "AXWindow"},
		{Index: 1, Parent: &root, Role: "AXTextField", Label: "Message", Frame: &screen.AXFrame{W: 90, H: 30}},
	}}
	snapshot.AssignKeys()
	fills := 0
	for _, action := range nativeActions(snapshot, nil) {
		if action.Kind == "fill" {
			fills++
			if strings.Contains(action.Text, "\n") {
				t.Fatalf("a single-line field was given a line break: %q", action.Text)
			}
		}
	}
	if fills != len(worstCases) {
		t.Fatalf("fills = %d, want %d", fills, len(worstCases))
	}
}

func baselineRun(t *testing.T, app *fakeApp, trace *Replay) *NativeOutcome {
	t.Helper()
	app.started = time.Now()
	outcome, err := RunNativeSession(context.Background(), nativeConfig(t), provider.Set{}, NativeOptions{
		Subject: run.Subject{Name: "fake"}, Harness: app, Replay: trace, Baseline: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func driftChecks(outcome *NativeOutcome) string {
	names := []string{}
	for _, finding := range outcome.Session.Findings {
		if strings.HasPrefix(finding.Check, "structure_drift") {
			names = append(names, finding.Check+":"+finding.Feature)
		}
	}
	return strings.Join(names, " ")
}

func TestBaselineComparesEveryStateAndMatchesItself(t *testing.T) {
	approved := runFake(t, &fakeApp{}, 3, nil).Trace
	outcome := baselineRun(t, &fakeApp{}, approved)
	if got := driftChecks(outcome); got != "" {
		t.Fatalf("an unchanged app drifted: %s", got)
	}
	if outcome.Compared != len(approved.Steps)+1 || outcome.Replayed != len(approved.Steps) {
		t.Fatalf("compared %d, replayed %d of %d steps", outcome.Compared, outcome.Replayed, len(approved.Steps))
	}
}

func TestBaselineReportsAnUnrequestedRow(t *testing.T) {
	approved := runFake(t, &fakeApp{}, 2, nil).Trace
	root := 0
	label := "Model"
	app := &fakeApp{extra: &screen.AXElement{Parent: &root, Role: "AXStaticText", Label: label, Frame: &screen.AXFrame{X: 700, Y: 40, W: 60, H: 16}}}
	got := driftChecks(baselineRun(t, app, approved))
	if !strings.Contains(got, sensors.DriftAdded+`:StaticText "Model"`) {
		t.Fatalf("drift = %q", got)
	}
}

func TestBaselineWaitsForASlowScreenToArrive(t *testing.T) {
	approved := runFake(t, &fakeApp{}, 1, nil).Trace
	root := 0
	app := &fakeApp{
		extra:      &screen.AXElement{Parent: &root, Role: "AXStaticText", Label: "Syncing", Frame: &screen.AXFrame{X: 700, Y: 40, W: 60, H: 16}},
		extraReads: 6,
	}
	if got := driftChecks(baselineRun(t, app, approved)); got != "" {
		t.Fatalf("a screen that arrived in time drifted: %s", got)
	}
}

func TestBaselineStopsWhereItsStepIsGone(t *testing.T) {
	approved := &Replay{Steps: []ReplayStep{
		{Action: guiAction{ID: "press:AXButton|Gone#0", Kind: "press", Element: "AXButton|Gone#0", Label: "press Gone"}},
		{Action: guiAction{ID: "press:AXButton|Open#0", Kind: "press", Element: "AXButton|Open#0", Label: "press Open"}},
	}}
	app := &fakeApp{}
	outcome := baselineRun(t, app, approved)
	if !outcome.Drifted || len(app.presses) != 0 || len(outcome.Trace.Steps) != 0 {
		t.Fatalf("drifted %t, presses %v, trace %d", outcome.Drifted, app.presses, len(outcome.Trace.Steps))
	}
	if got := driftChecks(outcome); !strings.Contains(got, "step: press Gone") {
		t.Fatalf("drift = %q", got)
	}
}

func TestLoadReplayMissingIsNotAnError(t *testing.T) {
	replay, err := LoadReplay(filepath.Join(t.TempDir(), "none.json"))
	if err != nil || replay != nil {
		t.Fatalf("replay = %+v, err = %v", replay, err)
	}
}

func TestMeasuredFindingsEscapeTheReportingCap(t *testing.T) {
	session := &run.Session{}
	for index := 0; index < run.MaxReportedPerGroup+3; index++ {
		session.Findings = append(session.Findings, run.Finding{
			ID: string(rune('a' + index)), Group: "native:chat", Check: sensors.AXClipped, Class: "truncation",
			Verified: "deterministic", Reported: true,
		})
	}
	run.ApplyReportingPolicy(session)
	for _, finding := range session.Findings {
		if !finding.Reported {
			t.Fatalf("a measured finding was capped: %+v", finding)
		}
	}
}
