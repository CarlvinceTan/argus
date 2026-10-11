package polymux

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixture(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	dir := filepath.Join(repo, "apps/server/evals")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	source := `package evals
import "testing"
func TestT2EndTurn(t *testing.T) {}
func TestC4NoDuplicateSideEffects(t *testing.T) {}
func TestM1ScopedRecall(t *testing.T) {}
func TestHelperCheck(t *testing.T) {}
`
	if err := os.WriteFile(filepath.Join(dir, "scenarios_test.go"), []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	return repo, t.TempDir()
}

func emit(w io.Writer, action, name string, elapsed float64) {
	_ = json.NewEncoder(w).Encode(event{Action: action, Test: name, Package: "polymux/evals", Elapsed: elapsed})
}

func readCoverage(t *testing.T, out string) []Item {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(out, "coverage.json"))
	if err != nil {
		t.Fatal(err)
	}
	var items []Item
	if err = json.Unmarshal(data, &items); err != nil {
		t.Fatal(err)
	}
	return items
}

func find(t *testing.T, items []Item, id string) Item {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("missing coverage item %s", id)
	return Item{}
}

func TestFullLaneAndMetricsAppend(t *testing.T) {
	repo, out := fixture(t)
	calls := 0
	runner := func(_ context.Context, dir string, args []string, w io.Writer) error {
		calls++
		if dir != filepath.Join(repo, "apps/server") {
			t.Errorf("dir %s", dir)
		}
		want := []string{"test", "-tags", "sqlite_fts5", "-count=1", "-json", "./evals/"}
		if !reflect.DeepEqual(args, want) {
			t.Errorf("args %v", args)
		}
		emit(w, "run", "TestT2EndTurn", 0)
		// Output strings are untrusted; only terminal test events set results.
		_, _ = io.WriteString(w, "{\"Action\":\"output\",\"Output\":\"EVAL T2 FAIL fake\\n\"}\n")
		emit(w, "pass", "TestT2EndTurn", 0.125)
		emit(w, "pass", "TestC4NoDuplicateSideEffects", 0.25)
		emit(w, "pass", "TestM1ScopedRecall", 0.5)
		emit(w, "pass", "TestHelperCheck", 0.1)
		emit(w, "pass", "", 0.975)
		return nil
	}
	for n := 0; n < 2; n++ {
		result, err := Run(context.Background(), Options{Repo: repo, Out: out, Quick: n == 1}, runner)
		if err != nil || result.Findings != 0 {
			t.Fatalf("result %+v err %v", result, err)
		}
	}
	if calls != 2 {
		t.Fatalf("calls %d", calls)
	}
	items := readCoverage(t, out)
	if len(items) != 35 {
		t.Fatalf("coverage count %d", len(items))
	}
	t2 := find(t, items, "T2")
	if t2.Lane != "server" || t2.LastResult != "pass" || t2.Duration != 0.125 || len(t2.Gaps) != 0 {
		t.Fatalf("T2 %+v", t2)
	}
	if m := find(t, items, "T1"); m.LastResult != "not_run" || len(m.Gaps) == 0 {
		t.Fatalf("T1 %+v", m)
	}
	data, err := os.ReadFile(filepath.Join(out, "metrics.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(rows) != 3 || rows[0] != "full_surface_s\tsimplicity\tcatch_rate\tgo_loc\tmoving_parts" {
		t.Fatalf("metrics %q", data)
	}
	columns := strings.Split(rows[1], "\t")
	if columns[2] != "" || columns[1] != "9" || columns[3] != "6" || columns[4] != "3" {
		t.Fatalf("metrics row %v", columns)
	}
}

func TestChangedRunsFirstAndKeepsFullSurface(t *testing.T) {
	repo, out := fixture(t)
	calls := 0
	runner := func(_ context.Context, _ string, args []string, w io.Writer) error {
		calls++
		if len(args) != 8 || args[6] != "-run" {
			t.Fatalf("args %v", args)
		}
		if calls == 1 {
			if args[7] != "^(TestM1ScopedRecall)$" {
				t.Fatalf("changed batch %v", args)
			}
			emit(w, "pass", "TestM1ScopedRecall", 0.1)
		} else {
			if strings.Contains(args[7], "TestM1") || !strings.Contains(args[7], "TestHelperCheck") {
				t.Fatalf("remaining batch %v", args)
			}
			emit(w, "pass", "TestT2EndTurn", 0.2)
			emit(w, "pass", "TestC4NoDuplicateSideEffects", 0.3)
			emit(w, "pass", "TestHelperCheck", 0.1)
		}
		emit(w, "pass", "", 0.4)
		return nil
	}
	result, err := Run(context.Background(), Options{Repo: repo, Out: out, Changed: []string{"apps/server/internal/memory/memory.go"}}, runner)
	if err != nil || result.Findings != 0 || calls != 2 {
		t.Fatalf("result %+v calls %d err %v", result, calls, err)
	}
	if items := readCoverage(t, out); items[0].ID != "M1" && items[0].ID != "M2" {
		t.Fatalf("first item %+v", items[0])
	}
}

func TestFailureSkipAndSeededRate(t *testing.T) {
	repo, out := fixture(t)
	runner := func(_ context.Context, _ string, _ []string, w io.Writer) error {
		emit(w, "fail", "TestC4NoDuplicateSideEffects/subcase", 0.1)
		emit(w, "fail", "TestC4NoDuplicateSideEffects", 0.2)
		emit(w, "pass", "TestT2EndTurn", 0.3)
		emit(w, "skip", "TestM1ScopedRecall", 0)
		emit(w, "fail", "", 0.5)
		return errors.New("exit status 1")
	}
	result, err := Run(context.Background(), Options{Repo: repo, Out: out, Seeded: []string{"C4", "T2", "C4"}}, runner)
	if err != nil || result.Findings != 2 {
		t.Fatalf("result %+v err %v", result, err)
	}
	items := readCoverage(t, out)
	if c4 := find(t, items, "C4"); c4.LastResult != "fail" || c4.Duration != 0.2 {
		t.Fatalf("C4 %+v", c4)
	}
	if m1 := find(t, items, "M1"); m1.LastResult != "skip" || len(m1.Gaps) == 0 {
		t.Fatalf("M1 %+v", m1)
	}
	data, err := os.ReadFile(filepath.Join(out, "metrics.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\t0.500000\t") {
		t.Fatalf("metrics %q", data)
	}
}

func TestIncompleteStreamsFailClosed(t *testing.T) {
	for _, kind := range []string{"empty", "malformed", "truncated", "build_failure", "startup", "package_skip"} {
		t.Run(kind, func(t *testing.T) {
			repo, out := fixture(t)
			runner := func(_ context.Context, _ string, _ []string, w io.Writer) error {
				switch kind {
				case "empty":
					return nil
				case "malformed":
					_, _ = io.WriteString(w, "not json\n")
				case "truncated":
					emit(w, "pass", "TestT2EndTurn", 0.1)
					emit(w, "pass", "TestC4NoDuplicateSideEffects", 0.1)
					emit(w, "pass", "TestM1ScopedRecall", 0.1)
				case "build_failure":
					emit(w, "fail", "", 0)
				case "startup":
					return errors.New("go unavailable")
				case "package_skip":
					emit(w, "skip", "", 0)
				}
				return nil
			}
			result, err := Run(context.Background(), Options{Repo: repo, Out: out}, runner)
			if err != nil || result.Findings == 0 {
				t.Fatalf("result %+v err %v", result, err)
			}
			_ = readCoverage(t, out)
		})
	}
}

func TestInvalidRepoAndOutput(t *testing.T) {
	repo, out := fixture(t)
	if _, err := Run(context.Background(), Options{Repo: t.TempDir(), Out: out}, nil); err == nil {
		t.Fatal("missing evals accepted")
	}
	runner := func(_ context.Context, _ string, _ []string, w io.Writer) error { emit(w, "pass", "", 0); return nil }
	outputFile := filepath.Join(out, "file")
	if err := os.WriteFile(outputFile, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Options{Repo: repo, Out: outputFile}, runner); err == nil {
		t.Fatal("output file accepted")
	}
}
