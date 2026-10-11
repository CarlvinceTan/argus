package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const pinned = "0123456789abcdef0123456789abcdef01234567"

type fakeRunner struct {
	calls, prepared, cleaned int
	fail                     bool
	samples                  []Pass
}

func (f *fakeRunner) Prepare(_ context.Context, b Build, out string) (string, func() error, error) {
	f.prepared++
	return out, func() error { f.cleaned++; return nil }, nil
}
func (f *fakeRunner) Pass(_ context.Context, work, out string) (Pass, error) {
	f.calls++
	if f.fail {
		return Pass{}, errors.New("infrastructure unavailable")
	}
	return f.samples[(f.calls-1)%len(f.samples)], nil
}
func corpus(t *testing.T) (Options, Manifest) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "benchmark", "polymux")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bug.patch"), []byte("fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: 1, Builds: []Build{
		{ID: "clean", Commit: pinned, Layer: "server", Expected: []Finding{}},
		{ID: "bug", Commit: pinned, Layer: "agent", Patch: "bug.patch", Expected: []Finding{{Class: "reply_integrity", Flow: "T2", Test: "TestT2EndTurn"}}},
	}}
	if err := writeJSON(filepath.Join(dir, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}
	ticks := 0
	clock := func() time.Time {
		seconds := []int64{0, 11, 11, 22, 22, 28}[ticks%6]
		ticks++
		return time.Unix(seconds, 0)
	}
	return Options{Corpus: dir, Out: filepath.Join(root, "results"), Argus: "../..", Now: clock}, m
}
func pass(wall float64, findings ...Finding) Pass {
	outcomes := map[string]string{"TestT2EndTurn": "pass"}
	if len(findings) > 0 {
		outcomes["TestT2EndTurn"] = "fail"
	}
	return Pass{Resources: []string{"process:argus", "process:go-test", "host:local"}, WallSeconds: wall, LaneSeconds: map[string]float64{"server": wall}, Findings: findings, Outcomes: outcomes}
}
func TestRunThreeRoundsMedianFlakyAndAppendOnly(t *testing.T) {
	opts, m := corpus(t)
	finding := m.Builds[1].Expected[0]
	// Round walls are 11, 11, 6. Taking per-build medians would give 7.
	fake := &fakeRunner{samples: []Pass{pass(1), pass(10, finding), pass(8, finding), pass(3, finding), pass(4), pass(2)}}
	got, err := Run(context.Background(), opts, fake)
	if err != nil {
		t.Fatal(err)
	}
	if fake.calls != 6 || fake.prepared != 6 || fake.cleaned != 6 {
		t.Fatalf("isolation counts: %+v", fake)
	}
	if got.WallSeconds != 11 || got.LaneSeconds["server"] != 11 || got.Planted != 1 || got.Caught != 0 || got.FalsePositives != 1 || len(got.Flaky) == 0 {
		t.Fatalf("summary: %+v", got)
	}
	history := filepath.Join(filepath.Dir(opts.Corpus), "history.tsv")
	before, err := os.ReadFile(history)
	if err != nil {
		t.Fatal(err)
	}
	opts.Out += "-again"
	got, err = Run(context.Background(), opts, fake)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(history)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(after), string(before)) || len(after) <= len(before) {
		t.Fatal("history was rewritten")
	}
	previous, err := lastHistory(history, got.Host)
	if err != nil || previous.Caught != got.Caught {
		t.Fatalf("history: %+v %v", previous, err)
	}
	var saved Summary
	data, err := os.ReadFile(filepath.Join(opts.Out, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &saved); err != nil || len(saved.Samples) != 6 || saved.Regressions == nil {
		t.Fatalf("summary evidence: %v", err)
	}
}
func TestStableFindingExactMatchAndGate(t *testing.T) {
	opts, m := corpus(t)
	ticks := int64(0)
	step := int64(1)
	opts.Now = func() time.Time { ticks += step; return time.Unix(ticks, 0) }
	finding := m.Builds[1].Expected[0]
	f := &fakeRunner{samples: []Pass{pass(1), pass(2, finding)}}
	got, err := Run(context.Background(), opts, f)
	if err != nil || got.Caught != 1 || len(got.Flaky) != 0 {
		t.Fatalf("stable score: %+v %v", got, err)
	}
	wrong := finding
	wrong.Flow = "T4"
	f.samples = []Pass{pass(2, wrong), pass(3, wrong)}
	step = 2
	opts.Out += "-regression"
	got, err = Run(context.Background(), opts, f)
	if err == nil || len(got.Regressions) != 3 {
		t.Fatalf("gate failed: %+v %v", got, err)
	}
	// Repeating the unexplained run must still compare with the accepted row.
	opts.Out += "-repeat"
	got, err = Run(context.Background(), opts, f)
	if err == nil || len(got.Regressions) != 3 {
		t.Fatalf("unexplained row advanced baseline: %+v %v", got, err)
	}
	// Failed gates still retain evidence and append one completed-run row.
	opts.Out += "-reason"
	opts.Reason = "Additional checks explain the slower run in this PR."
	if got, err = Run(context.Background(), opts, f); err != nil || len(got.Regressions) != 3 {
		t.Fatalf("explained regression: %+v %v", got, err)
	}
	opts.Out += "-accepted"
	opts.Reason = ""
	if got, err = Run(context.Background(), opts, f); err != nil || len(got.Regressions) != 0 {
		t.Fatalf("explained row did not advance baseline: %+v %v", got, err)
	}
}

func TestUnobservedEvalsDoNotScoreOrAppend(t *testing.T) {
	for _, kind := range []string{"clean skip", "clean empty", "plant missing", "plant skip"} {
		t.Run(kind, func(t *testing.T) {
			opts, m := corpus(t)
			clean, planted := pass(1), pass(1, m.Builds[1].Expected[0])
			switch kind {
			case "clean skip":
				clean.Outcomes["TestT2EndTurn"] = "skip"
			case "clean empty":
				clean.Outcomes = nil
			case "plant missing":
				planted.Outcomes = map[string]string{"TestOther": "pass"}
			case "plant skip":
				planted.Outcomes["TestT2EndTurn"] = "skip"
			}
			fake := &fakeRunner{samples: []Pass{clean, planted}}
			if _, err := Run(t.Context(), opts, fake); err == nil {
				t.Fatal("unobservable corpus scored")
			}
			if fake.cleaned != fake.prepared {
				t.Fatal("scratch worktree leaked")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(opts.Corpus), "history.tsv")); !os.IsNotExist(err) {
				t.Fatal("unobservable corpus appended history")
			}
		})
	}
}

func TestArgusRootWalksUpFromCorpus(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "benchmark", "polymux")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/carlvincetan/argus\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, err := argusRoot("", dir); err != nil || got != root {
		t.Fatalf("walk up: %q %v", got, err)
	}
	if _, err := argusRoot(dir, dir); err == nil {
		t.Fatal("explicit override silently walked up")
	}
}
func TestInfrastructureFailureCleansAndDoesNotScore(t *testing.T) {
	opts, _ := corpus(t)
	fake := &fakeRunner{fail: true}
	if _, err := Run(context.Background(), opts, fake); err == nil {
		t.Fatal("infrastructure failure passed")
	}
	if fake.cleaned != 1 {
		t.Fatal("scratch worktree leaked")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(opts.Corpus), "history.tsv")); !os.IsNotExist(err) {
		t.Fatal("incomplete benchmark appended history")
	}
}
func TestManifestValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"duplicate", func(m *Manifest) { m.Builds[1].ID = m.Builds[0].ID }},
		{"unpinned", func(m *Manifest) { m.Builds[0].Commit = "main" }},
		{"escape", func(m *Manifest) { m.Builds[1].Patch = "../bug.patch" }},
		{"unsupported lane", func(m *Manifest) { m.Builds[0].Layer = "macOS" }},
		{"no expectation", func(m *Manifest) { m.Builds[1].Expected = nil }},
		{"incomplete finding", func(m *Manifest) { m.Builds[1].Expected[0].Flow = "" }},
		{"unanchored primary flow", func(m *Manifest) { m.Builds[1].Flow = "X4" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts, m := corpus(t)
			test.mutate(&m)
			if err := writeJSON(filepath.Join(opts.Corpus, "manifest.json"), m); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadManifest(opts.Corpus); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}
func TestSeedCorpus(t *testing.T) {
	m, err := LoadManifest("../../benchmark/polymux")
	if err != nil {
		t.Fatal(err)
	}
	clean, planted := 0, 0
	for _, b := range m.Builds {
		if b.Patch == "" {
			clean++
		} else {
			planted++
		}
	}
	if clean != 3 || planted != 5 {
		t.Fatal(fmt.Sprintf("seed counts: clean=%d planted=%d", clean, planted))
	}
}
