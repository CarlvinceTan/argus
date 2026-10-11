package triage

import (
	"path/filepath"
	"testing"
)

func label(finding, check, class string, prob float64, verdict Verdict, note string) Label {
	return Label{
		FindingID:  finding,
		Check:      check,
		Class:      class,
		DefectProb: prob,
		Verdict:    verdict,
		Note:       note,
	}
}

func TestStoreKeepsLatestVerdictPerFinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "labels.jsonl")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Append(label("f1", "border_gap", "border", 0.8, Accepted, "real")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := store.Append(label("f1", "border_gap", "border", 0.8, Rejected, "changed my mind")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := store.Append(label("f2", "flicker", "animation", 0.7, Accepted, "")); err != nil {
		t.Fatalf("append: %v", err)
	}
	byFinding, err := store.ByFinding()
	if err != nil {
		t.Fatalf("byFinding: %v", err)
	}
	if len(byFinding) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(byFinding))
	}
	if byFinding["f1"].Verdict != Rejected {
		t.Fatalf("latest verdict not used: %+v", byFinding["f1"])
	}
	if byFinding["f1"].Note != "changed my mind" {
		t.Fatalf("note not stored: %+v", byFinding["f1"])
	}
}

func TestStoreRejectsUnknownVerdict(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "labels.jsonl"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Append(Label{FindingID: "f1", Verdict: "maybe"}); err == nil {
		t.Fatal("expected an unknown verdict to be rejected")
	}
}

func TestBuildSuppressesQuietChecksAndKeepsGoodOnes(t *testing.T) {
	labels := []Label{}
	// A check that is usually wrong on this codebase.
	for index := 0; index < 8; index++ {
		labels = append(labels, label(string(rune('a'+index)), "screen", "other", 0.7, Rejected, "model speculation"))
	}
	// A check that is usually right.
	for index := 0; index < 8; index++ {
		labels = append(labels, label(string(rune('m'+index)), "progress_label_mismatch", "inconsistency", 0.9, Accepted, "bar disagrees with label"))
	}
	calibration := Build(labels, DefaultOptions())
	if len(calibration.Suppressed) != 1 || calibration.Suppressed[0] != "screen" {
		t.Fatalf("expected only the quiet check to be suppressed: %+v", calibration.Suppressed)
	}
	if len(calibration.Promoted) != 1 || calibration.Promoted[0] != "progress_label_mismatch" {
		t.Fatalf("expected the reliable check to be promoted: %+v", calibration.Promoted)
	}
	if stat := calibration.Checks["screen"]; stat.Precision >= 0.3 {
		t.Fatalf("precision should be low: %+v", stat)
	}
}

func TestBuildNeedsEnoughLabelsBeforeChangingAnything(t *testing.T) {
	labels := []Label{
		label("f1", "screen", "other", 0.7, Rejected, ""),
		label("f2", "screen", "other", 0.7, Rejected, ""),
	}
	calibration := Build(labels, DefaultOptions())
	if len(calibration.Suppressed) != 0 {
		t.Fatalf("two labels must not suppress a check: %+v", calibration.Suppressed)
	}
	if calibration.IsEmpty() && len(calibration.Checks) == 0 {
		t.Fatal("statistics should still be recorded")
	}
}

func TestApplyBlendsRatherThanVetoes(t *testing.T) {
	calibration := Build(append(
		repeat(label("a", "screen", "other", 0.6, Rejected, ""), 6),
		repeat(label("b", "border_gap", "border", 0.9, Accepted, ""), 6)...,
	), DefaultOptions())

	quiet := calibration.Apply("", "screen", "other", 0.9)
	if quiet.Adjusted >= 0.9 {
		t.Fatalf("a quiet check should be adjusted down: %+v", quiet)
	}
	if quiet.Adjusted < 0.2 {
		t.Fatalf("a measured candidate must not be vetoed outright: %+v", quiet)
	}
	if !quiet.Suppressed {
		t.Fatalf("expected the check to be marked suppressed: %+v", quiet)
	}
	if quiet.Adjusted < 0.2 {
		t.Fatalf("a measured candidate must not be vetoed outright: %+v", quiet)
	}

	good := calibration.Apply("", "border_gap", "border", 0.5)
	if good.Adjusted <= 0.5 {
		t.Fatalf("a promoted check should be adjusted up: %+v", good)
	}

	// A thin history nudges rather than decides: one label on one finding must
	// not be able to push a confident measurement out of the backlog.
	thin := Build([]Label{label("x", "screen", "other", 0.9, Rejected, "")}, DefaultOptions())
	state := thin.Apply("", "screen", "other", 0.9)
	if state.Adjusted < 0.7 {
		t.Fatalf("one label moved the probability too far: %+v", state)
	}
	if state.Adjusted >= 0.9 {
		t.Fatalf("one label should still move the probability: %+v", state)
	}
}

func TestApplyIsInertWithoutLabels(t *testing.T) {
	calibration := Empty()
	state := calibration.Apply("", "border_gap", "border", 0.62)
	if state.Adjusted != 0.62 || state.Suppressed || state.Promoted {
		t.Fatalf("empty calibration changed a probability: %+v", state)
	}
}

func TestCalibrationRoundTrips(t *testing.T) {
	calibration := Build(repeat(label("a", "screen", "other", 0.7, Rejected, ""), 6), DefaultOptions())
	path := filepath.Join(t.TempDir(), "calibration.json")
	if err := calibration.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Summary() != calibration.Summary() {
		t.Fatalf("summary mismatch: %q vs %q", loaded.Summary(), calibration.Summary())
	}
	if state := loaded.Apply("", "screen", "other", 0.9); !state.Suppressed {
		t.Fatal("loaded calibration did not suppress")
	}
}

func TestLoadMissingFileIsInert(t *testing.T) {
	loaded, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.IsEmpty() {
		t.Fatal("missing calibration should be inert")
	}
}

func TestSummariseCountsVerdictsAndNotes(t *testing.T) {
	labels := []Label{
		label("a", "border_gap", "border", 0.8, Accepted, "yes"),
		label("b", "e", "f", 0.8, Misclassified, "right issue, wrong class"),
		label("c", "g", "h", 0.8, Duplicate, ""),
	}
	report := Summarise(labels)
	if report.Labels != 3 || report.NoteCount != 2 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if report.ByVerdict["accepted"] != 1 || report.ByVerdict["duplicate"] != 1 {
		t.Fatalf("verdict counts wrong: %+v", report.ByVerdict)
	}
	// A misclassification is still a real issue, so it counts as accepted.
	if report.ByCheck["border_gap"].Accepted != 1 {
		t.Fatalf("expected accepted count for border_gap: %+v", report.ByCheck)
	}
}

func repeat(label Label, times int) []Label {
	out := make([]Label, 0, times)
	for index := 0; index < times; index++ {
		clone := label
		clone.FindingID = label.FindingID + string(rune('0'+index))
		out = append(out, clone)
	}
	return out
}

func TestCalibrationIsScopedPerSubjectKind(t *testing.T) {
	labels := []Label{}
	// A terminal history where the "screen" check was usually wrong.
	for index := 0; index < 8; index++ {
		rejected := label(string(rune('a'+index)), "screen", "other", 0.9, Rejected, "model speculation")
		rejected.Kind = "tui"
		labels = append(labels, rejected)
	}
	calibration := Build(labels, DefaultOptions())

	tui := calibration.Apply("tui", "screen", "other", 0.9)
	if tui.Adjusted >= 0.9 || !tui.Suppressed {
		t.Fatalf("terminal history should discount the check: %+v", tui)
	}
	// The same check on a GUI subject has no history of its own, and must not
	// inherit the terminal verdict.
	gui := calibration.Apply("gui", "screen", "other", 0.9)
	if gui.Adjusted != 0.9 || gui.Suppressed {
		t.Fatalf("gui findings must not inherit terminal history: %+v", gui)
	}
}
