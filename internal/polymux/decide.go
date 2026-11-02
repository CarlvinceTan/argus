package polymux

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/carlvincetan/argus/internal/jev"
)

// The decision filter narrows the journeys the area rules kept by asking a
// decision model one question per journey: can this change affect it? It only
// ever errs toward running: a journey is left out when the model rates it under
// minAffected, and anything the model can't judge still runs. The idea and its
// safety rules follow tester-army/e2e's decisionFilter.
const (
	minAffected     = 0.2
	maxDiffBytes    = 60 << 10
	maxSummaryItems = 40
	maxFlowNote     = 240
	journeysPerCall = 20 // the providers' question limit per call
)

// wholeSurface are files that set up every journey (fixtures, intents, the
// build's dependencies), so a change to one is never left to the model.
var wholeSurface = []string{
	"e2e/intents.json", "e2e/fixtures/*", "scripts/run*",
	"Package.swift", "Package.resolved",
	"package-lock.json", "pnpm-lock.yaml", "yarn.lock", "*.xcodeproj/*",
}

// secretPaths are listed by name but never sent as content.
var secretPaths = []string{".env*", "*.pem", "*.key", "*.p12", "*secret*", "*credential*"}

// Change is what a branch changed since its base, as the decision filter sees it.
type Change struct {
	Base      string   `json:"base"`
	Files     []string `json:"files"`
	Untracked []string `json:"untracked,omitempty"`
	Diff      string   `json:"-"`
}

// Paths is every changed path, which is what the area rules take.
func (c Change) Paths() []string {
	return append(append([]string{}, c.Files...), c.Untracked...)
}

// ChangesSince measures the working tree against the merge base with ref, so a
// branch is judged on its own commits and not on what main gained since.
// Untracked files are named but their content isn't sent.
func ChangesSince(ctx context.Context, repo, ref string) (Change, error) {
	git := func(args ...string) (string, error) {
		command := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
		out, err := command.Output()
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok && len(exit.Stderr) > 0 {
				return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(exit.Stderr)))
			}
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return string(out), nil
	}
	base, err := git("merge-base", ref, "HEAD")
	if err != nil {
		return Change{}, fmt.Errorf("--changed-since %s: %w (a shallow clone needs fetch-depth 0)", ref, err)
	}
	change := Change{Base: strings.TrimSpace(base)}
	names, err := git("diff", "--name-only", change.Base)
	if err != nil {
		return Change{}, err
	}
	change.Files = strings.Fields(names)
	untracked, err := git("ls-files", "--others", "--exclude-standard")
	if err != nil {
		return Change{}, err
	}
	change.Untracked = strings.Fields(untracked)
	spec := []string{"diff", "--no-color", "--no-ext-diff", change.Base, "--", "."}
	for _, pattern := range secretPaths {
		spec = append(spec, ":(exclude,glob)**/"+pattern)
	}
	if change.Diff, err = git(spec...); err != nil {
		return Change{}, err
	}
	return change, nil
}

// Decider answers typed questions; provider.DecisionProvider satisfies it.
type Decider interface {
	Decide(ctx context.Context, state any, questions map[string]jev.Question, repeats int, meta map[string]any) (jev.Result, error)
}

// Selection is what the decision filter kept and why it left the rest out.
type Selection struct {
	Kept    []Journey
	LeftOut map[string]string // journey id -> reason
	Note    string            // one line for the log and swarm.md
	Calls   int
	Tokens  int
	CostUSD float64
}

// DecideJourneys asks decider which of journeys change can affect. It never
// fails: when it can't judge, every journey runs and Note says why.
func DecideJourneys(ctx context.Context, repo string, journeys []Journey, change Change, decider Decider) Selection {
	selection := Selection{Kept: journeys, LeftOut: map[string]string{}}
	if len(journeys) == 0 || decider == nil {
		return selection
	}
	everything := func(format string, args ...any) Selection {
		selection.Note = fmt.Sprintf("decision filter ran every journey: "+format, args...)
		return selection
	}
	if len(change.Paths()) == 0 {
		return everything("nothing changed since %s", short(change.Base))
	}
	for _, file := range change.Paths() {
		for _, pattern := range wholeSurface {
			if pathMatches(file, pattern) {
				return everything("%s sets up every journey", file)
			}
		}
	}
	if len(change.Diff) > maxDiffBytes {
		return everything("the diff is %d kB, over the %d kB the model is shown", len(change.Diff)>>10, maxDiffBytes>>10)
	}

	// A journey whose own flows or baseline changed runs without asking, like a
	// test whose file changed.
	changed := map[string]bool{}
	for _, file := range change.Paths() {
		changed[file] = true
	}
	type asked struct {
		id      string
		journey Journey
	}
	pending := []asked{}
	for index, journey := range journeys {
		if !owns(journey, changed) {
			pending = append(pending, asked{fmt.Sprintf("journey_%02d", index), journey})
		}
	}
	if len(pending) == 0 {
		return everything("every journey's own flows or baseline changed")
	}
	// Each call carries the diff and only its own journeys, and a failed call
	// keeps just its own journeys running.
	var failure error
	before := callCount(decider)
	for start := 0; start < len(pending); start += journeysPerCall {
		batch := pending[start:min(start+journeysPerCall, len(pending))]
		summaries := map[string]journeySummary{}
		questions := map[string]jev.Question{}
		for _, item := range batch {
			summaries[item.id] = summarise(repo, item.journey)
			questions[item.id] = jev.Noul(fmt.Sprintf(
				"Could this change alter what journey %s (%s) shows or does? The journey is described under journeys.%s: its flows, the controls they use and the screens it reaches. Answer yes unless the change clearly cannot reach any of them.",
				item.id, item.journey.ID, item.id))
		}
		state := map[string]any{
			"task": "Decide which UI journeys of the Polymux app a code change can affect, so the rest can be skipped on this pull request. Files listed without a diff (new or withheld) still count: judge them by name.",
			"change": map[string]any{
				"files":     change.Files,
				"untracked": change.Untracked,
				"diff":      change.Diff,
			},
			"journeys": summaries,
		}
		result, err := decider.Decide(ctx, state, questions, 1, map[string]any{"purpose": "change-filter"})
		selection.Calls++
		if err != nil {
			failure = err
			continue
		}
		selection.Tokens += result.Usage.InputTokens + result.Usage.OutputTokens
		selection.CostUSD += result.Usage.CostUSD
		for _, item := range batch {
			answer, ok := result.Answers[item.id]
			if !ok {
				continue // unjudged journeys run
			}
			if p := answer.P(); p < minAffected {
				selection.LeftOut[item.journey.ID] = fmt.Sprintf("%.0f%% likely to be affected", p*100)
			}
		}
	}
	if after := callCount(decider); after > before {
		selection.Calls = after - before // the provider may split a call further
	}
	if failure != nil && selection.Tokens == 0 && len(selection.LeftOut) == 0 {
		return everything("the model call failed (%v)", failure)
	}
	selection.Kept = nil
	for _, journey := range journeys {
		if _, out := selection.LeftOut[journey.ID]; !out {
			selection.Kept = append(selection.Kept, journey)
		}
	}
	selection.Note = fmt.Sprintf("decision kept %d of %d journeys, left out %d (%s, %s tokens, $%.4f)",
		len(selection.Kept), len(journeys), len(selection.LeftOut), plural(selection.Calls, "call"), thousands(selection.Tokens), selection.CostUSD)
	if failure != nil {
		selection.Note += fmt.Sprintf("; a call failed (%v), so its journeys ran", failure)
	}
	return selection
}

// callCount reads a provider's own call counter when it has one.
func callCount(decider Decider) int {
	if counter, ok := decider.(interface{ Calls() int }); ok {
		return counter.Calls()
	}
	return 0
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func owns(journey Journey, changed map[string]bool) bool {
	if changed["e2e/baselines/"+journey.ID+".json"] {
		return true
	}
	for _, flow := range journey.Flows {
		if changed["e2e/flows/"+flow] {
			return true
		}
	}
	return false
}

// journeySummary is what the model knows about a journey: a few hundred bytes
// instead of the flow sources and baselines, which are megabytes.
type journeySummary struct {
	Name     string   `json:"name"`
	Flows    []string `json:"flows"`
	Controls []string `json:"controls,omitempty"`
	Screens  []string `json:"screens,omitempty"`
}

var controlName = regexp.MustCompile(`name:\s*"([^"]+)"`)

func summarise(repo string, journey Journey) journeySummary {
	summary := journeySummary{Name: journey.ID}
	controls := map[string]bool{}
	for _, flow := range journey.Flows {
		source, err := os.ReadFile(filepath.Join(repo, "e2e/flows", flow))
		if err != nil {
			summary.Flows = append(summary.Flows, flow)
			continue
		}
		summary.Flows = append(summary.Flows, flow+": "+flowNote(string(source)))
		for _, match := range controlName.FindAllStringSubmatch(string(source), -1) {
			controls[match[1]] = true
		}
	}
	summary.Controls = capped(controls)
	summary.Screens = baselineLabels(repo, journey.ID)
	return summary
}

// flowNote is a flow's leading comment: what it checks, in the author's words.
func flowNote(source string) string {
	var note []string
	for _, line := range strings.Split(source, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "//") {
			break
		}
		if text := strings.TrimSpace(strings.TrimPrefix(line, "//")); !strings.HasPrefix(text, "@") {
			note = append(note, text)
		}
	}
	joined := strings.Join(note, " ")
	if runes := []rune(joined); len(runes) > maxFlowNote {
		joined = string(runes[:maxFlowNote]) + "..."
	}
	return joined
}

// baselineLabels names the screens and controls a journey's approved baseline
// reaches, capped for the model.
func baselineLabels(repo, id string) []string {
	return capped(screenLabels(repo, id))
}

// screenLabels is every scene, action and element label in a journey's approved
// baseline. Rows read "role|label|frame|flags".
func screenLabels(repo, id string) map[string]bool {
	raw, err := os.ReadFile(filepath.Join(repo, "e2e/baselines", id+".json"))
	if err != nil {
		return nil
	}
	type state struct {
		Rows []string `json:"rows"`
	}
	var baseline struct {
		Scene string `json:"scene"`
		State *state `json:"state"`
		Steps []struct {
			Scene  string `json:"scene"`
			State  *state `json:"state"`
			Action struct {
				Label string `json:"label"`
			} `json:"action"`
		} `json:"steps"`
	}
	if json.Unmarshal(raw, &baseline) != nil {
		return nil
	}
	labels := map[string]bool{}
	add := func(text string) {
		if text = strings.Trim(strings.TrimSpace(text), ", "); text != "" {
			labels[text] = true
		}
	}
	rows := func(s *state) {
		if s == nil {
			return
		}
		for _, row := range s.Rows {
			if parts := strings.Split(row, "|"); len(parts) > 1 {
				add(parts[1])
			}
		}
	}
	add(baseline.Scene)
	rows(baseline.State)
	for _, step := range baseline.Steps {
		add(step.Scene)
		add(step.Action.Label)
		rows(step.State)
	}
	return labels
}

func capped(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for item := range set {
		out = append(out, item)
	}
	sort.Strings(out)
	if len(out) > maxSummaryItems {
		out = out[:maxSummaryItems]
	}
	return out
}

func short(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}

func thousands(tokens int) string {
	if tokens < 1000 {
		return fmt.Sprint(tokens)
	}
	return fmt.Sprintf("%.1fk", float64(tokens)/1000)
}
