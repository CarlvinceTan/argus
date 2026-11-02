// Package swarm runs many journeys side by side, each with its own driver and
// app, and merges what they report into one backlog.
package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/carlvincetan/argus/internal/agent"
	"github.com/carlvincetan/argus/internal/polymux"
	"github.com/carlvincetan/argus/internal/run"
)

// Result is one journey's outcome. Error means the run itself failed, which says
// nothing about the app.
type Result struct {
	Journey  polymux.Journey `json:"journey"`
	Dir      string          `json:"dir"`
	Session  *run.Session    `json:"-"`
	Findings int             `json:"findings"`
	Crashed  bool            `json:"crashed"`
	Replayed int             `json:"replayed"`
	Drifted  bool            `json:"drifted"`
	Blind    int             `json:"blind_steps"`
	// Compared counts states checked against a baseline; zero means it has none.
	Compared int        `json:"compared"`
	Perf     agent.Perf `json:"perf"`
	Calls    int        `json:"model_calls"`
	Seconds  float64    `json:"seconds"`
	Error    string     `json:"error,omitempty"`
	// Retried says what the first attempt saw, when a journey needed a second.
	Retried string `json:"retried,omitempty"`
}

// Issue is one problem and every journey that saw it.
type Issue struct {
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
	Check    string   `json:"check"`
	Severity string   `json:"severity"`
	Score    float64  `json:"score"`
	Element  string   `json:"element,omitempty"`
	Still    string   `json:"still,omitempty"`
	Journeys []string `json:"journeys"`
	// Stale names an intent that used to cover this and needs a fresh decision.
	Stale string `json:"stale_intent,omitempty"`
	// Standing is new, known or reopened against an issue ledger, when one is given.
	Standing string `json:"standing,omitempty"`
	Since    string `json:"since,omitempty"`
	SeenRuns int    `json:"seen_runs,omitempty"`
}

// Report is a whole swarm run.
type Report struct {
	Results []Result `json:"results"`
	Issues  []Issue  `json:"issues"`
	Seconds float64  `json:"seconds"`
	// Honoured counts findings a confirmed intent kept out of the backlog.
	Honoured map[string]int `json:"honoured_intents,omitempty"`
	// Stale lists intents whose sources changed, whether or not they matched.
	Stale []string `json:"stale_intents,omitempty"`
	// Unused lists intents nothing matched on a whole-surface run.
	Unused []string `json:"unused_intents,omitempty"`
	// Baselines counts journeys by how they compared.
	Matched int `json:"baselines_matched"`
	Drifted int `json:"baselines_drifted"`
	Missing int `json:"baselines_missing"`
	// Load is the machine's load average and core count at the start: a number
	// measured on a busy machine says little about the app.
	Load  float64 `json:"load"`
	Cores int     `json:"cores"`
	// Selection is the decision filter's summary, and LeftOut the journeys it
	// skipped with the reason for each, so a skipped journey is never silent.
	Selection string            `json:"selection,omitempty"`
	LeftOut   map[string]string `json:"left_out,omitempty"`
	// Resolved lists ledger issues this run looked for and no longer saw.
	Resolved []string `json:"resolved,omitempty"`
}

// Timing is how fast one action answered across every journey that tried it.
type Timing struct {
	Action   string `json:"action"`
	Samples  int    `json:"samples"`
	MedianMS int64  `json:"median_ms"`
	MaxMS    int64  `json:"max_ms"`
}

// Timings ranks actions by median response. A single sample is noise on a shared
// machine, so only actions seen several times are ranked.
func (r Report) Timings() []Timing {
	const minSamples = 3
	samples := map[string][]int64{}
	for _, result := range r.Results {
		for _, response := range result.Perf.Responses {
			samples[response.Action] = append(samples[response.Action], response.MS)
		}
	}
	out := []Timing{}
	for action, values := range samples {
		if len(values) < minSamples {
			continue
		}
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		out = append(out, Timing{action, len(values), values[len(values)/2], values[len(values)-1]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MedianMS != out[j].MedianMS {
			return out[i].MedianMS > out[j].MedianMS
		}
		return out[i].Action < out[j].Action
	})
	return out
}

func (r Report) perfMarkdown(text *strings.Builder) {
	timings := r.Timings()
	var slowest, growth int64
	var idleCPU float64
	worst := agent.Response{}
	for _, result := range r.Results {
		slowest = max(slowest, result.Perf.SlowestMS)
		growth = max(growth, result.Perf.GrowthKB)
		if result.Perf.IdleCPU > idleCPU {
			idleCPU = result.Perf.IdleCPU
		}
		for _, response := range result.Perf.Responses {
			if response.MS > worst.MS {
				worst = response
			}
		}
	}
	fmt.Fprintf(text, "\n## Performance\n\nMeasured at load %.1f on %d cores. Compare these against a baseline run; alone they are not findings.\n\n", r.Load, r.Cores)
	fmt.Fprintf(text, "- Slowest single response: %d ms, %s\n- Longest the main thread held a request: %d ms\n- Largest memory growth in a journey: %d KB\n- Highest idle CPU: %.1f%%\n",
		worst.MS, worst.Action, slowest, growth, idleCPU)
	if len(timings) == 0 {
		return
	}
	text.WriteString("\n| Action | Samples | Median ms | Max ms |\n| --- | --- | --- | --- |\n")
	for index, timing := range timings {
		if index == 10 {
			break
		}
		fmt.Fprintf(text, "| %s | %d | %d | %d |\n", timing.Action, timing.Samples, timing.MedianMS, timing.MaxMS)
	}
}

// Runner executes one journey and writes its evidence under dir.
type Runner func(ctx context.Context, journey polymux.Journey, dir string) Result

// Run executes the journeys with at most parallel running at once. One journey
// failing does not stop the others.
func Run(ctx context.Context, journeys []polymux.Journey, parallel int, out string, runner Runner) Report {
	start := time.Now()
	if parallel < 1 {
		parallel = 1
	}
	results := make([]Result, len(journeys))
	slots := make(chan struct{}, parallel)
	var wait sync.WaitGroup
	for index, journey := range journeys {
		wait.Add(1)
		go func(index int, journey polymux.Journey) {
			defer wait.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			dir := filepath.Join(out, journey.ID)
			begun := time.Now()
			result := Result{Journey: journey, Dir: dir, Error: "cancelled before it started"}
			if ctx.Err() == nil {
				result = runner(ctx, journey, dir)
				result.Journey, result.Dir = journey, dir
			}
			result.Seconds = time.Since(begun).Seconds()
			results[index] = result
		}(index, journey)
	}
	wait.Wait()
	// One at a time, alone, so that a real regression fails again and a one-off clears.
	// Whatever the first attempt saw stays in the report, so flakiness isn't hidden.
	for index, first := range results {
		reason := retryReason(first)
		if reason == "" || ctx.Err() != nil {
			continue
		}
		dir := filepath.Join(out, first.Journey.ID)
		begun := time.Now()
		second := runner(ctx, first.Journey, dir)
		second.Journey, second.Dir, second.Seconds = first.Journey, dir, time.Since(begun).Seconds()
		second.Retried = reason
		results[index] = second
	}
	report := Report{
		Results: results, Issues: Merge(results), Honoured: honoured(results),
		Seconds: time.Since(start).Seconds(),
	}
	report.countBaselines()
	return report
}

func retryReason(result Result) string {
	switch {
	case result.Error == "cancelled before it started":
		return ""
	case result.Error != "":
		return result.Error
	case result.Session != nil:
		for _, finding := range result.Session.Findings {
			if finding.Reported && strings.HasPrefix(finding.Check, "structure_drift") {
				return finding.Detail
			}
		}
	}
	return ""
}

func (r *Report) countBaselines() {
	for _, result := range r.Results {
		switch {
		case result.Error != "":
		case result.Compared == 0:
			r.Missing++
		case drifted(result):
			r.Drifted++
		default:
			r.Matched++
		}
	}
}

func drifted(result Result) bool {
	if result.Session == nil {
		return false
	}
	for _, finding := range result.Session.Findings {
		if finding.Reported && strings.HasPrefix(finding.Check, "structure_drift") {
			return true
		}
	}
	return false
}

func honoured(results []Result) map[string]int {
	counts := map[string]int{}
	for _, result := range results {
		if result.Session == nil {
			continue
		}
		for _, finding := range result.Session.Findings {
			if id := tagged(finding, run.TagIntent); id != "" {
				counts[id]++
			}
		}
	}
	return counts
}

func tagged(finding run.Finding, prefix string) string {
	for _, tag := range finding.Tags {
		if id, ok := strings.CutPrefix(tag, prefix); ok {
			return id
		}
	}
	return ""
}

// Merge folds the same problem seen on several journeys into one issue.
func Merge(results []Result) []Issue {
	issues := map[string]*Issue{}
	order := []string{}
	for _, result := range results {
		if result.Session == nil {
			continue
		}
		for _, finding := range result.Session.Findings {
			if !finding.Reported {
				continue
			}
			// Measured findings name their element; a model's name the screen.
			subject := finding.Feature
			if subject == "" {
				subject = finding.Brief
			}
			key := finding.Check + "|" + finding.Class + "|" + subject
			issue, seen := issues[key]
			if !seen {
				issue = &Issue{
					Title: finding.Title, Detail: finding.Detail, Check: finding.Check, Element: finding.Feature,
					Severity: finding.SeverityName, Score: finding.SeverityScore,
				}
				if finding.Still != "" {
					issue.Still = filepath.Join(result.Journey.ID, finding.Still)
				}
				issues[key] = issue
				order = append(order, key)
			}
			if finding.SeverityScore > issue.Score {
				issue.Severity, issue.Score = finding.SeverityName, finding.SeverityScore
			}
			if id := tagged(finding, run.TagStaleIntent); id != "" {
				issue.Stale = id
			}
			if !contains(issue.Journeys, result.Journey.ID) {
				issue.Journeys = append(issue.Journeys, result.Journey.ID)
			}
		}
	}
	out := make([]Issue, 0, len(order))
	for _, key := range order {
		out = append(out, *issues[key])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return len(out[i].Journeys) > len(out[j].Journeys)
	})
	return out
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// blockingScore is noticeable or worse; below it an issue is a hint for review.
const blockingScore = 1.5

// Blocking counts issues that should fail a run.
func (r Report) Blocking() int {
	count := 0
	for _, issue := range r.Issues {
		if issue.Score >= blockingScore {
			count++
		}
	}
	return count
}

// Failed counts journeys that could not run; they make the result inconclusive.
func (r Report) Failed() int {
	count := 0
	for _, result := range r.Results {
		if result.Error != "" {
			count++
		}
	}
	return count
}

// Write saves swarm.json and swarm.md.
func (r Report) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "swarm.json"), raw, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "swarm.md"), []byte(r.markdown()), 0o644)
}

func (r Report) retriesMarkdown(text *strings.Builder) {
	retried := []string{}
	for _, result := range r.Results {
		if result.Retried != "" {
			retried = append(retried, fmt.Sprintf("- %s first saw: %s", result.Journey.ID, result.Retried))
		}
	}
	if len(retried) > 0 {
		fmt.Fprintf(text, "\n## Retried\n\n%s\n", strings.Join(retried, "\n"))
	}
}

func (r Report) leftOutMarkdown(text *strings.Builder) {
	if r.Selection == "" {
		return
	}
	fmt.Fprintf(text, "\n## Change filter\n\n%s\n", r.Selection)
	ids := make([]string, 0, len(r.LeftOut))
	for id := range r.LeftOut {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		text.WriteString("\n")
	}
	for _, id := range ids {
		fmt.Fprintf(text, "- %s left out: %s\n", id, r.LeftOut[id])
	}
}

func (r Report) intentsMarkdown(text *strings.Builder) {
	if len(r.Honoured)+len(r.Stale)+len(r.Unused) == 0 {
		return
	}
	text.WriteString("\n## Intents\n\n")
	ids := make([]string, 0, len(r.Honoured))
	for id := range r.Honoured {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(text, "- %s kept %d findings out of the backlog\n", id, r.Honoured[id])
	}
	for _, id := range r.Stale {
		fmt.Fprintf(text, "- %s is stale: confirm it or remove it\n", id)
	}
	for _, id := range r.Unused {
		fmt.Fprintf(text, "- %s matched nothing on a whole-surface run: it may no longer be needed\n", id)
	}
}

func (r Report) markdown() string {
	var text strings.Builder
	fmt.Fprintf(&text, "# Argus swarm\n\n%d journeys, %d issues, %d failed runs, %.0f s\n\n", len(r.Results), len(r.Issues), r.Failed(), r.Seconds)
	fmt.Fprintf(&text, "Baselines: %d matched, %d drifted, %d with none.\n\n", r.Matched, r.Drifted, r.Missing)
	text.WriteString("## Issues\n\n")
	if len(r.Issues) == 0 {
		text.WriteString("None.\n")
	}
	for _, issue := range r.Issues {
		fmt.Fprintf(&text, "- **[%s] %s**%s seen in %d: %s\n  %s\n", issue.Severity, issue.Title, r.standingMarkdown(issue), len(issue.Journeys), strings.Join(issue.Journeys, ", "), issue.Detail)
		if issue.Still != "" {
			fmt.Fprintf(&text, "  ![evidence](%s)\n", issue.Still)
		}
		if strings.HasPrefix(issue.Check, "structure_drift") {
			fmt.Fprintf(&text, "  If this change is wanted: `argus baseline approve --repo . --run THIS_RUN --journey %s --reason WHY`\n", issue.Journeys[0])
		} else if issue.Stale != "" {
			fmt.Fprintf(&text, "  Intended before, but its sources changed. If it still is: `argus intent confirm --repo . --id %s`\n", issue.Stale)
		} else if issue.Element != "" {
			fmt.Fprintf(&text, "  If this is intended: `argus intent add --repo . --id NAME --check %s --element %q --reason WHY --paths SOURCES`\n", issue.Check, issue.Element)
		}
	}
	r.resolvedMarkdown(&text)
	r.retriesMarkdown(&text)
	r.leftOutMarkdown(&text)
	r.intentsMarkdown(&text)
	r.perfMarkdown(&text)
	text.WriteString("\n## Journeys\n\n| Journey | Findings | Replayed | Model calls | Seconds | Status |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, result := range r.Results {
		status := "ok"
		switch {
		case result.Error != "":
			status = "run failed: " + result.Error
		case result.Crashed:
			status = "app crashed"
		case result.Blind > 0:
			status = fmt.Sprintf("%d steps without a screenshot", result.Blind)
		case result.Drifted:
			status = "ok, drifted from replay"
		case result.Retried != "":
			status = "ok on retry"
		}
		fmt.Fprintf(&text, "| %s | %d | %d | %d | %.0f | %s |\n", result.Journey.ID, result.Findings, result.Replayed, result.Calls, result.Seconds, status)
	}
	return text.String()
}
