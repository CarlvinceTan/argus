package swarm

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/carlvincetan/argus/internal/polymux"
)

func issue(check, element string, score float64, journeys ...string) Issue {
	return Issue{Title: check, Check: check, Element: element, Severity: "noticeable", Score: score, Journeys: journeys}
}

func ran(ids ...string) []Result {
	out := []Result{}
	for _, id := range ids {
		out = append(out, Result{Journey: polymux.Journey{ID: id}})
	}
	return out
}

func TestIssueKeyReadsNumbersAsHash(t *testing.T) {
	a := IssueKey(issue("structure_ax_small_target", `Button "Cancel Scheduled check 3"`, 1))
	b := IssueKey(issue("structure_ax_small_target", `Button "Cancel Scheduled check 14"`, 1))
	if a != b || a != `structure_ax_small_target Button "Cancel Scheduled check #"` {
		t.Fatalf("keys %q and %q", a, b)
	}
}

func TestLedgerTellsNewFromKnownAndOnlyRecordChangesIt(t *testing.T) {
	day1 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	ledger := &Ledger{}
	first := Report{Results: ran("chat"), Issues: []Issue{issue("structure_ax_unlabelled", "unnamed Button", 2, "chat")}}
	ledger.Compare(&first)
	if first.Issues[0].Standing != StandingNew || first.BlockingNew() != 1 {
		t.Fatalf("first sighting: %+v", first.Issues[0])
	}
	if len(ledger.Issues) != 0 {
		t.Fatal("Compare changed the ledger")
	}
	ledger.Record(first, day1)

	second := Report{Results: ran("chat"), Issues: []Issue{
		issue("structure_ax_unlabelled", "unnamed Button", 2, "chat"),
		issue("structure_ax_clipped", `Button "Send"`, 2.5, "chat"),
	}}
	ledger.Compare(&second)
	if second.Issues[0].Standing != StandingKnown || second.Issues[0].Since != "2026-11-01" || second.Issues[0].SeenRuns != 1 {
		t.Fatalf("known issue: %+v", second.Issues[0])
	}
	if second.Issues[1].Standing != StandingNew || second.Blocking() != 2 || second.BlockingNew() != 1 {
		t.Fatalf("new issue: %+v, blocking %d new %d", second.Issues[1], second.Blocking(), second.BlockingNew())
	}
}

func TestLedgerResolvesOnlyWhereTheRunLooked(t *testing.T) {
	ledger := &Ledger{}
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ledger.Record(Report{Issues: []Issue{issue("structure_ax_inert", `Button "Collapse"`, 2, "chat", "sidebar")}}, day)

	elsewhere := Report{Results: ran("profile")}
	ledger.Compare(&elsewhere)
	if len(elsewhere.Resolved) != 0 {
		t.Fatalf("resolved by a run that never opened its journeys: %v", elsewhere.Resolved)
	}
	blind := Report{Results: []Result{{Journey: polymux.Journey{ID: "chat"}, Blind: 2}, {Journey: polymux.Journey{ID: "sidebar"}, Error: "Control needs Accessibility"}}}
	ledger.Compare(&blind)
	if len(blind.Resolved) != 0 {
		t.Fatalf("resolved by a run that couldn't see: %v", blind.Resolved)
	}
	clean := Report{Results: ran("sidebar")}
	ledger.Compare(&clean)
	if want := []string{`structure_ax_inert Button "Collapse"`}; !reflect.DeepEqual(clean.Resolved, want) {
		t.Fatalf("resolved %v, want %v", clean.Resolved, want)
	}
	ledger.Record(clean, day.AddDate(0, 0, 1))
	if got := ledger.Issues[0]; got.Status != "resolved" || got.Resolved != "2026-11-02" {
		t.Fatalf("after recording: %+v", got)
	}

	back := Report{Results: ran("chat"), Issues: []Issue{issue("structure_ax_inert", `Button "Collapse"`, 2, "chat")}}
	ledger.Compare(&back)
	if back.Issues[0].Standing != StandingReopened || back.BlockingNew() != 1 {
		t.Fatalf("a returning issue must fail again: %+v", back.Issues[0])
	}
	ledger.Record(back, day.AddDate(0, 0, 2))
	if got := ledger.Issues[0]; got.Status != "open" || got.Reopened != 1 || got.Runs != 2 || got.Resolved != "" {
		t.Fatalf("reopened row: %+v", got)
	}
}

func TestLedgerRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "issues.json")
	missing, err := LoadLedger(path)
	if err != nil || len(missing.Issues) != 0 {
		t.Fatalf("missing ledger: %v %v", missing, err)
	}
	ledger := &Ledger{}
	ledger.Record(Report{Issues: []Issue{issue("b", "x", 1, "j"), issue("a", "y", 1, "j")}}, time.Now())
	if err := ledger.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadLedger(path)
	if err != nil || len(loaded.Issues) != 2 || loaded.Issues[0].Key != "a y" {
		t.Fatalf("loaded %+v %v", loaded, err)
	}
}
