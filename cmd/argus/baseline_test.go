package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carlvincetan/argus/internal/agent"
	"github.com/carlvincetan/argus/internal/intent"
	"github.com/carlvincetan/argus/internal/polymux"
	"github.com/carlvincetan/argus/internal/sensors"
	"github.com/carlvincetan/argus/internal/swarm"
)

// approvalRun lays out a finished run with one good, one crashed and one blind journey.
func approvalRun(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	report := swarm.Report{Results: []swarm.Result{
		{Journey: polymux.Journey{ID: "good"}},
		{Journey: polymux.Journey{ID: "crashed"}, Crashed: true},
		{Journey: polymux.Journey{ID: "blind"}, Blind: 2},
		{Journey: polymux.Journey{ID: "broken"}, Error: "no window"},
	}}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "swarm.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	state := sensors.State{W: 800, H: 600, Rows: []sensors.StateRow{{Role: "AXButton", Name: "Send", W: 80, H: 30}}}
	trace := &agent.Replay{Start: "a", State: &state}
	if err := trace.Save(filepath.Join(dir, "good", "trace.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "good", "states"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "good", "states", "0.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func approve(args ...string) (int, string, string) {
	var out, errors bytes.Buffer
	code := baselineCommand(append([]string{"approve"}, args...), &out, &errors)
	return code, out.String(), errors.String()
}

func TestApproveNeedsAReasonAndACleanRun(t *testing.T) {
	repo, images := t.TempDir(), t.TempDir()
	run := approvalRun(t)
	common := []string{"--repo", repo, "--run", run, "--images", images}
	refused := map[string][]string{
		"no reason":  append(append([]string{}, common...), "--journey", "good"),
		"blank":      append(append([]string{}, common...), "--journey", "good", "--reason", "  "),
		"no journey": append(append([]string{}, common...), "--reason", "x"),
		"both":       append(append([]string{}, common...), "--reason", "x", "--all", "--journey", "good"),
		"crashed":    append(append([]string{}, common...), "--reason", "x", "--journey", "crashed"),
		"blind":      append(append([]string{}, common...), "--reason", "x", "--journey", "blind"),
		"failed":     append(append([]string{}, common...), "--reason", "x", "--journey", "broken"),
		"not in run": append(append([]string{}, common...), "--reason", "x", "--journey", "other"),
	}
	for name, args := range refused {
		if code, _, _ := approve(args...); code == 0 {
			t.Fatalf("%s was approved", name)
		}
	}
	if entries, _ := filepath.Glob(filepath.Join(repo, baselineDir, "*")); len(entries) != 0 {
		t.Fatalf("a refused approval wrote %v", entries)
	}
}

func TestApproveRecordsWhyAndTheScreenshots(t *testing.T) {
	repo, images := t.TempDir(), t.TempDir()
	run := approvalRun(t)
	code, out, errors := approve("--repo", repo, "--run", run, "--images", images, "--journey", "good", "--reason", "new composer")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errors)
	}
	if !strings.Contains(out, "new baseline") {
		t.Fatalf("out = %q", out)
	}
	trace, err := agent.LoadReplay(baselinePath(repo, "good"))
	if err != nil || trace == nil {
		t.Fatalf("trace %+v, err %v", trace, err)
	}
	if trace.Reason != "new composer" || trace.Approved == "" {
		t.Fatalf("trace = %+v", trace)
	}
	if _, err := os.Stat(filepath.Join(images, "good", "0.png")); err != nil {
		t.Fatal(err)
	}
	var list bytes.Buffer
	if code := baselineCommand([]string{"list", "--repo", repo}, &list, &bytes.Buffer{}); code != 0 || !strings.Contains(list.String(), "new composer") {
		t.Fatalf("list %d: %s", code, list.String())
	}
}

func TestApproveAllKeepsTheCleanJourneysAndStillFails(t *testing.T) {
	repo, images := t.TempDir(), t.TempDir()
	run := approvalRun(t)
	if code, _, errors := approve("--repo", repo, "--run", run, "--images", images, "--all", "--reason", "first baseline"); code == 0 || !strings.Contains(errors, "--confirm") {
		t.Fatalf("--all ran without a second run: %q", errors)
	}
	code, out, errors := approve("--repo", repo, "--run", run, "--confirm", run, "--images", images, "--all", "--reason", "first baseline")
	if code == 0 {
		t.Fatal("refused journeys must fail the command")
	}
	if !strings.Contains(out, "approved good") || !strings.Contains(errors, "3 journeys were not approved") {
		t.Fatalf("out %q, errors %q", out, errors)
	}
	if trace, err := agent.LoadReplay(baselinePath(repo, "good")); err != nil || trace == nil {
		t.Fatalf("the clean journey wasn't saved: %v", err)
	}
}

func TestApproveRefusesWhatDidntReproduce(t *testing.T) {
	repo, images := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "e2e"), 0o755); err != nil {
		t.Fatal(err)
	}
	run, other := approvalRun(t), approvalRun(t)
	state := sensors.State{W: 800, H: 600, Rows: []sensors.StateRow{{Role: "AXStaticText", Name: "Couldn't save", W: 80, H: 16}}}
	trace := &agent.Replay{Start: "a", State: &state}
	if err := trace.Save(filepath.Join(other, "good", "trace.json")); err != nil {
		t.Fatal(err)
	}
	args := []string{"--repo", repo, "--run", run, "--confirm", other, "--images", images, "--journey", "good", "--reason", "x"}
	code, _, errors := approve(args...)
	if code == 0 || !strings.Contains(errors, "disagree") {
		t.Fatalf("a state that didn't reproduce was approved: %q", errors)
	}
	if err := os.WriteFile(filepath.Join(repo, "View.swift"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ledger, err := intent.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []string{"structure_drift_*"} {
		if err := ledger.Add(intent.Intent{ID: "known", Check: check, Element: "*", Journey: "good", Reason: "varies", Paths: []string{"View.swift"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ledger.Save(); err != nil {
		t.Fatal(err)
	}
	if code, _, errors := approve(args...); code != 0 {
		t.Fatalf("known variation blocked an approval: %q", errors)
	}
}

func TestReapprovingSaysWhatChanged(t *testing.T) {
	old := sensors.State{W: 800, H: 600, Rows: []sensors.StateRow{{Role: "AXButton", Name: "Send", W: 80, H: 30}}}
	next := sensors.State{W: 800, H: 600, Rows: []sensors.StateRow{{Role: "AXStaticText", Name: "Model", W: 40, H: 16}}}
	got := summarise(&agent.Replay{State: &old}, &agent.Replay{State: &next})
	if got != "1 added, 1 gone, 0 moved" {
		t.Fatalf("summary = %q", got)
	}
}
