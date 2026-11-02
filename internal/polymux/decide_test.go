package polymux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/carlvincetan/argus/internal/jev"
)

// fakeDecider answers each journey question with the probability given for the
// journey named in it, and leaves journeys it has no number for unanswered.
type fakeDecider struct {
	p         map[string]float64
	err       error
	state     any
	questions map[string]jev.Question
}

func (f *fakeDecider) Decide(_ context.Context, state any, questions map[string]jev.Question, _ int, _ map[string]any) (jev.Result, error) {
	f.state, f.questions = state, questions
	if f.err != nil {
		return jev.Result{}, f.err
	}
	result := jev.Result{Answers: map[string]jev.Answer{}, Usage: jev.Usage{InputTokens: 17500, OutputTokens: 100, CostUSD: 0.0007}}
	for id, question := range questions {
		for journey, p := range f.p {
			if strings.Contains(question.Instructions, "("+journey+")") {
				result.Answers[id] = jev.Answer{ID: id, Type: "noul", Noul: p}
			}
		}
	}
	return result, nil
}

func decideRepo(t *testing.T) (string, []Journey) {
	t.Helper()
	repo := journeyRepo(t)
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("e2e/flows/fixture-profile.js", "// @fixture --verify-fixture --verify-profile\n// Renaming a bot updates the sidebar.\nawait app.click({ role: \"button\", name: \"Rename\" });\n")
	if err := os.MkdirAll(filepath.Join(repo, "e2e/baselines"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("e2e/baselines/fixture-profile.json", `{"scene": "Profile", "state": {"rows": ["AXButton|Rename|0,0,10,10|", "AXTextField|Name, ,|0,0,10,10|"]},
		"steps": [{"scene": "Profile edit", "action": {"label": "Rename"}, "state": {"rows": ["AXButton|Save|0,0,1,1|"]}}]}`)
	journeys, err := PlanJourneys(repo, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return repo, journeys
}

func ids(journeys []Journey) []string {
	out := []string{}
	for _, journey := range journeys {
		out = append(out, journey.ID)
	}
	return out
}

func TestDecideJourneysLeavesOutOnlyUnlikely(t *testing.T) {
	repo, journeys := decideRepo(t)
	change := Change{Base: "abc", Files: []string{"apps/Polymux/Sources/Views/ChatPane.swift"}, Diff: "+ Text(\"hi\")\n"}
	decider := &fakeDecider{p: map[string]float64{
		"fixture-chat-place": 0.9,
		"fixture-chat-long":  0.19, // under the bar: left out
		// fixture-profile is unanswered: it runs
	}}
	selection := DecideJourneys(context.Background(), repo, journeys, change, decider)
	if got, want := ids(selection.Kept), []string{"fixture-chat-place", "fixture-profile"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
	if reason := selection.LeftOut["fixture-chat-long"]; reason != "19% likely to be affected" || len(selection.LeftOut) != 1 {
		t.Fatalf("left out %v", selection.LeftOut)
	}
	if want := "decision kept 2 of 3 journeys, left out 1 (1 call, 17.6k tokens, $0.0007)"; selection.Note != want {
		t.Fatalf("note %q, want %q", selection.Note, want)
	}
	// The model sees each journey's own words, controls and screens, not its sources.
	raw, _ := json.Marshal(decider.state)
	for _, want := range []string{"Renaming a bot updates the sidebar.", `"Rename"`, `"Profile edit"`, `"Save"`, `"Name"`, "ChatPane.swift"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("state lacks %s: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "@fixture") {
		t.Errorf("state carries the fixture line: %s", raw)
	}
}

func TestDecideJourneysErrsTowardRunning(t *testing.T) {
	repo, journeys := decideRepo(t)
	all := ids(journeys)
	low := map[string]float64{"fixture-chat-place": 0.01, "fixture-chat-long": 0.01, "fixture-profile": 0.01}
	cases := []struct {
		name   string
		change Change
		err    error
		note   string
		asked  int
	}{
		{"model fails", Change{Files: []string{"a.swift"}, Diff: "x"}, errors.New("timeout"), "the model call failed (timeout)", 3},
		{"lockfile", Change{Files: []string{"a.swift", "Package.resolved"}, Diff: "x"}, nil, "Package.resolved sets up every journey", 0},
		{"fixture", Change{Files: []string{"e2e/fixtures/bots.json"}, Diff: "x"}, nil, "e2e/fixtures/bots.json sets up every journey", 0},
		{"huge diff", Change{Files: []string{"a.swift"}, Diff: strings.Repeat("x", maxDiffBytes+1)}, nil, "over the 60 kB", 0},
		{"no change", Change{Base: "0123456789"}, nil, "nothing changed since 01234567", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decider := &fakeDecider{p: low, err: tc.err}
			selection := DecideJourneys(context.Background(), repo, journeys, tc.change, decider)
			if got := ids(selection.Kept); !reflect.DeepEqual(got, all) || len(selection.LeftOut) != 0 {
				t.Fatalf("kept %v left out %v, want everything", got, selection.LeftOut)
			}
			if !strings.Contains(selection.Note, tc.note) {
				t.Fatalf("note %q lacks %q", selection.Note, tc.note)
			}
			if len(decider.questions) != tc.asked {
				t.Fatalf("asked %d questions, want %d", len(decider.questions), tc.asked)
			}
		})
	}
}

func TestDecideJourneysRunsJourneysWhoseOwnFilesChanged(t *testing.T) {
	repo, journeys := decideRepo(t)
	low := map[string]float64{"fixture-chat-place": 0.01, "fixture-chat-long": 0.01, "fixture-profile": 0.01}
	change := Change{Files: []string{"e2e/flows/fixture-chat-long.js", "e2e/baselines/fixture-profile.json", "a.swift"}, Diff: "x"}
	decider := &fakeDecider{p: low}
	selection := DecideJourneys(context.Background(), repo, journeys, change, decider)
	if got, want := ids(selection.Kept), []string{"fixture-chat-long", "fixture-profile"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
	if len(decider.questions) != 1 {
		t.Fatalf("asked about %d journeys, want only the one whose files didn't change", len(decider.questions))
	}
}

func TestChangesSince(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, text string) {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write("Views/Chat.swift", "Text(\"old\")\n")
	write("config/.env.local", "TOKEN=old\n")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	git("checkout", "-q", "-b", "work")
	write("Views/Chat.swift", "Text(\"new\")\n")
	git("commit", "-q", "-am", "branch change")
	write("config/.env.local", "TOKEN=hunter2secret\n") // uncommitted, and secret
	write("Views/New.swift", "struct New {}\n")         // untracked
	git("checkout", "-q", "main")
	write("main-only.txt", "x\n")
	git("add", "main-only.txt")
	git("commit", "-q", "-m", "main moved on")
	git("checkout", "-q", "work")

	change, err := ChangesSince(context.Background(), repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Views/Chat.swift", "config/.env.local"}; !reflect.DeepEqual(change.Files, want) {
		t.Fatalf("files %v, want %v (main's own commits stay out)", change.Files, want)
	}
	if want := []string{"Views/New.swift"}; !reflect.DeepEqual(change.Untracked, want) {
		t.Fatalf("untracked %v, want %v", change.Untracked, want)
	}
	if !strings.Contains(change.Diff, `+Text("new")`) {
		t.Fatalf("diff lacks the branch change:\n%s", change.Diff)
	}
	if strings.Contains(change.Diff, "hunter2") || strings.Contains(change.Diff, ".env") {
		t.Fatalf("diff carries a secret file:\n%s", change.Diff)
	}
	if _, err := ChangesSince(context.Background(), repo, "no-such-ref"); err == nil || !strings.Contains(err.Error(), "--changed-since no-such-ref") {
		t.Fatalf("bad ref error = %v", err)
	}
}

// batchDecider rates every journey unlikely, records each call's journeys and
// fails the call numbered failOn.
type batchDecider struct {
	failOn int
	calls  [][]string
}

func (b *batchDecider) Decide(_ context.Context, state any, questions map[string]jev.Question, _ int, _ map[string]any) (jev.Result, error) {
	names := []string{}
	for id := range state.(map[string]any)["journeys"].(map[string]journeySummary) {
		names = append(names, id)
	}
	b.calls = append(b.calls, names)
	if len(b.calls) == b.failOn {
		return jev.Result{}, errors.New("rate limited")
	}
	result := jev.Result{Answers: map[string]jev.Answer{}, Usage: jev.Usage{InputTokens: 1000}}
	for id := range questions {
		result.Answers[id] = jev.Answer{ID: id, Type: "noul", Noul: 0.05}
	}
	return result, nil
}

func TestDecideJourneysBatchesAndKeepsAFailedBatch(t *testing.T) {
	journeys := []Journey{}
	for index := range 45 {
		journeys = append(journeys, Journey{ID: fmt.Sprintf("j%02d", index), Flows: []string{"missing.js"}})
	}
	decider := &batchDecider{failOn: 2}
	selection := DecideJourneys(context.Background(), t.TempDir(), journeys, Change{Files: []string{"a.swift"}, Diff: "x"}, decider)
	if len(decider.calls) != 3 || len(decider.calls[0]) != 20 || len(decider.calls[2]) != 5 {
		t.Fatalf("calls carried %d journeys", len(decider.calls))
	}
	// The second batch failed, so its 20 journeys run and the other 25 are left out.
	if len(selection.Kept) != 20 || selection.Kept[0].ID != "j20" || len(selection.LeftOut) != 25 {
		t.Fatalf("kept %v, left out %d", ids(selection.Kept), len(selection.LeftOut))
	}
	if want := "decision kept 20 of 45 journeys, left out 25 (3 calls, 2.0k tokens, $0.0000); a call failed (rate limited), so its journeys ran"; selection.Note != want {
		t.Fatalf("note %q, want %q", selection.Note, want)
	}
}
