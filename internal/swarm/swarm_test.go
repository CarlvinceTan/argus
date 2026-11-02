package swarm

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/carlvincetan/argus/internal/agent"
	"github.com/carlvincetan/argus/internal/polymux"
	"github.com/carlvincetan/argus/internal/run"
)

func finding(check, element string, score float64, reported bool) run.Finding {
	return run.Finding{Check: check, Class: "overlap", Feature: element, Title: check,
		SeverityScore: score, SeverityName: "major", Reported: reported}
}

func TestMergeFoldsTheSameProblem(t *testing.T) {
	results := []Result{
		{Journey: polymux.Journey{ID: "a"}, Session: &run.Session{Findings: []run.Finding{
			finding("overlap", "Button One", 2, true), finding("hidden", "Button Two", 4, false),
		}}},
		{Journey: polymux.Journey{ID: "b"}, Session: &run.Session{Findings: []run.Finding{
			finding("overlap", "Button One", 3, true), finding("clip", "Button Three", 1, true),
		}}},
		{Journey: polymux.Journey{ID: "c"}, Error: "no window"},
	}
	issues := Merge(results)
	if len(issues) != 2 {
		t.Fatalf("issues = %+v", issues)
	}
	first := issues[0]
	if first.Check != "overlap" || first.Score != 3 || len(first.Journeys) != 2 {
		t.Fatalf("first issue = %+v", first)
	}
}

func TestIntentsReachTheReport(t *testing.T) {
	honoured := finding("small", "Dismiss", 1, false)
	honoured.Intentional, honoured.Tags = true, []string{run.TagIntent + "tap"}
	reopened := finding("clip", "Send", 2.5, true)
	reopened.Tags = []string{run.TagStaleIntent + "edge"}
	journeys := []polymux.Journey{{ID: "a"}}
	report := Run(context.Background(), journeys, 1, t.TempDir(), func(context.Context, polymux.Journey, string) Result {
		return Result{Session: &run.Session{Findings: []run.Finding{honoured, reopened}}}
	})
	if report.Honoured["tap"] != 1 {
		t.Fatalf("honoured = %v", report.Honoured)
	}
	if len(report.Issues) != 1 || report.Issues[0].Stale != "edge" || report.Blocking() != 1 {
		t.Fatalf("issues = %+v", report.Issues)
	}
	text := report.markdown()
	for _, wanted := range []string{"argus intent confirm --repo . --id edge", "tap kept 1 findings"} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("report is missing %q:\n%s", wanted, text)
		}
	}
}

func TestTimingsRankRepeatedActionsOnly(t *testing.T) {
	response := func(action string, ms int64) agent.Response { return agent.Response{Action: action, MS: ms} }
	report := Report{Results: []Result{
		{Perf: agent.Perf{Responses: []agent.Response{response("open", 100), response("once", 9000)}}},
		{Perf: agent.Perf{Responses: []agent.Response{response("open", 900), response("send", 40)}}},
		{Perf: agent.Perf{Responses: []agent.Response{response("open", 200), response("send", 60)}}},
		{Perf: agent.Perf{Responses: []agent.Response{response("send", 50)}}},
	}}
	timings := report.Timings()
	if len(timings) != 2 || timings[0].Action != "open" || timings[0].MedianMS != 200 || timings[0].MaxMS != 900 {
		t.Fatalf("timings = %+v", timings)
	}
}

func TestRunRetriesOnceAndKeepsWhatItSaw(t *testing.T) {
	drift := run.Finding{Check: "structure_drift_added", Detail: "Model appeared", Reported: true}
	attempts := map[string]int{}
	var lock sync.Mutex
	journeys := []polymux.Journey{{ID: "flaky"}, {ID: "real"}, {ID: "clean"}}
	report := Run(context.Background(), journeys, 2, t.TempDir(), func(_ context.Context, journey polymux.Journey, _ string) Result {
		lock.Lock()
		attempts[journey.ID]++
		count := attempts[journey.ID]
		lock.Unlock()
		switch {
		case journey.ID == "real", journey.ID == "flaky" && count == 1:
			return Result{Session: &run.Session{Findings: []run.Finding{drift}}}
		}
		return Result{Session: &run.Session{}}
	})
	if attempts["flaky"] != 2 || attempts["real"] != 2 || attempts["clean"] != 1 {
		t.Fatalf("attempts = %v", attempts)
	}
	if report.Results[0].Retried != "Model appeared" || len(report.Results[0].Session.Findings) != 0 {
		t.Fatalf("flaky = %+v", report.Results[0])
	}
	if len(report.Results[1].Session.Findings) != 1 {
		t.Fatalf("a real regression cleared: %+v", report.Results[1])
	}
	if !strings.Contains(report.markdown(), "flaky first saw: Model appeared") {
		t.Fatalf("the retry wasn't reported:\n%s", report.markdown())
	}
}

func TestRunBoundsParallelismAndKeepsGoing(t *testing.T) {
	journeys := []polymux.Journey{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	var running, peak atomic.Int32
	report := Run(context.Background(), journeys, 2, t.TempDir(), func(_ context.Context, journey polymux.Journey, _ string) Result {
		now := running.Add(1)
		for {
			high := peak.Load()
			if now <= high || peak.CompareAndSwap(high, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		running.Add(-1)
		if journey.ID == "b" {
			return Result{Error: "instance failed"}
		}
		return Result{}
	})
	if peak.Load() != 2 {
		t.Fatalf("peak = %d, want 2", peak.Load())
	}
	if len(report.Results) != 4 || report.Failed() != 1 || report.Results[1].Journey.ID != "b" {
		t.Fatalf("report = %+v", report.Results)
	}
}

func TestRunMarksCancelledJourneysAsNotRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := Run(ctx, []polymux.Journey{{ID: "a"}}, 1, t.TempDir(), func(context.Context, polymux.Journey, string) Result {
		t.Error("a cancelled journey ran")
		return Result{}
	})
	if report.Failed() != 1 {
		t.Fatalf("report = %+v", report.Results)
	}
}
