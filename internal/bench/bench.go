// Package bench runs the versioned Polymux corpus. B1 covers the complete server
// eval suite; UI lanes are intentionally not scored until their drivers exist.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/carlvincetan/argus/internal/polymux"
)

type Finding struct {
	Class string `json:"class"`
	Flow  string `json:"flow"`
	Test  string `json:"test"`
}
type Build struct {
	ID       string    `json:"id"`
	Commit   string    `json:"commit"`
	Patch    string    `json:"patch,omitempty"`
	Layer    string    `json:"layer"`          // Bug surface; server and agent both use the B1 server lane.
	Flow     string    `json:"flow,omitempty"` // Optional primary flow, anchored in Expected.
	Expected []Finding `json:"expected"`
}
type Manifest struct {
	Version int     `json:"version"`
	Builds  []Build `json:"builds"`
}
type Pass struct {
	Resources   []string           `json:"resources"`
	Findings    []Finding          `json:"findings"`
	Outcomes    map[string]string  `json:"outcomes"`
	WallSeconds float64            `json:"wall_seconds"`
	LaneSeconds map[string]float64 `json:"lane_seconds"`
	ModelCalls  int                `json:"model_calls"`
	CostUSD     float64            `json:"cost_usd"`
}
type Sample struct {
	Build string `json:"build"`
	Run   int    `json:"run"`
	Pass
}
type Summary struct {
	OutputDir      string             `json:"output_dir"`
	Missed         []string           `json:"missed"`
	Resources      []string           `json:"resources"`
	Date           string             `json:"date"`
	ArgusCommit    string             `json:"argus_commit"`
	Host           string             `json:"host"`
	WallSeconds    float64            `json:"wall_seconds"`
	RunWallSeconds [3]float64         `json:"run_wall_seconds"`
	LaneSeconds    map[string]float64 `json:"lane_seconds"`
	Caught         int                `json:"caught"`
	Planted        int                `json:"planted"`
	FalsePositives int                `json:"false_positives"`
	Flaky          []string           `json:"flaky"`
	GoLOC          int                `json:"go_loc"`
	MovingParts    int                `json:"moving_parts"`
	ModelCalls     int                `json:"model_calls"`
	CostUSD        float64            `json:"cost_usd"`
	Reason         string             `json:"reason"`
	Samples        []Sample           `json:"samples"`
	Regressions    []string           `json:"regressions"`
}
type Options struct {
	Corpus, Polymux, Out, Argus, Reason string
	PassTimeout                         time.Duration    // Zero uses the local runner default.
	Now                                 func() time.Time // Optional clock for fake-run tests.
}

// Runner owns scratch isolation and the full-surface pass, allowing fake runs.
type Runner interface {
	Prepare(context.Context, Build, string) (string, func() error, error)
	Pass(context.Context, string, string) (Pass, error)
}

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func LoadManifest(corpus string) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(filepath.Join(corpus, "manifest.json"))
	if err != nil {
		return m, err
	}
	if err = json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	if m.Version != 1 || len(m.Builds) == 0 {
		return m, fmt.Errorf("manifest requires version 1 and at least one build")
	}
	ids := map[string]bool{}
	for _, b := range m.Builds {
		if !idPattern.MatchString(b.ID) || ids[b.ID] || !commitPattern.MatchString(b.Commit) {
			return m, fmt.Errorf("invalid or duplicate build %q or unpinned commit", b.ID)
		}
		ids[b.ID] = true
		if b.Layer != "server" && b.Layer != "agent" {
			return m, fmt.Errorf("unsupported B1 layer for %s", b.ID)
		}
		if (b.Patch == "") != (len(b.Expected) == 0) {
			return m, fmt.Errorf("%s: clean builds have no expected findings; planted builds require a patch and expected findings", b.ID)
		}
		if b.Patch != "" {
			if !filepath.IsLocal(b.Patch) {
				return m, fmt.Errorf("%s: patch must be inside corpus", b.ID)
			}
			root, e := filepath.EvalSymlinks(corpus)
			if e != nil {
				return m, e
			}
			patch, e := filepath.EvalSymlinks(filepath.Join(corpus, b.Patch))
			if e != nil {
				return m, e
			}
			rel, e := filepath.Rel(root, patch)
			if e != nil || !filepath.IsLocal(rel) {
				return m, fmt.Errorf("%s: patch escapes corpus", b.ID)
			}
		}
		seen := map[string]bool{}
		primaryFound := b.Flow == ""
		for _, f := range b.Expected {
			if f.Class == "" || f.Flow == "" || f.Test == "" || seen[key(f)] {
				return m, fmt.Errorf("%s: expected findings require unique class, flow and test", b.ID)
			}
			seen[key(f)] = true
			primaryFound = primaryFound || f.Flow == b.Flow
		}
		if !primaryFound {
			return m, fmt.Errorf("%s: primary flow must match an expected finding", b.ID)
		}
	}
	return m, nil
}

func Run(ctx context.Context, opts Options, runner Runner) (Summary, error) {
	var s Summary
	m, err := LoadManifest(opts.Corpus)
	if err != nil {
		return s, err
	}
	opts.Argus, err = argusRoot(opts.Argus, opts.Corpus)
	if err != nil {
		return s, err
	}
	commit, err := git(ctx, opts.Argus, "rev-parse", "HEAD")
	if err != nil {
		return s, err
	}
	commit = strings.TrimSpace(commit)
	status, err := git(ctx, opts.Argus, "status", "--porcelain")
	if err != nil {
		return s, err
	}
	if strings.TrimSpace(status) != "" {
		commit += "+dirty"
	}
	opts.Out, err = filepath.Abs(opts.Out)
	if err != nil {
		return s, err
	}
	// A second invocation under the same date retains every earlier artifact.
	if err = os.MkdirAll(filepath.Dir(opts.Out), 0755); err != nil {
		return s, err
	}
	if err = os.Mkdir(opts.Out, 0755); os.IsExist(err) {
		opts.Out, err = os.MkdirTemp(opts.Out, "invocation-")
	}
	if err != nil {
		return s, fmt.Errorf("create output directory: %w", err)
	}
	if runner == nil {
		runner = &LocalRunner{Repo: opts.Polymux, Corpus: opts.Corpus, Timeout: opts.PassTimeout}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	var walls [3]float64
	for round := 1; round <= 3; round++ {
		started := now()
		for _, b := range m.Builds {
			if err = ctx.Err(); err != nil {
				return s, err
			}
			out := filepath.Join(opts.Out, fmt.Sprintf("run-%d", round), b.ID)
			if err = os.MkdirAll(out, 0755); err != nil {
				return s, err
			}
			work, cleanup, e := runner.Prepare(ctx, b, out)
			if e != nil {
				return s, fmt.Errorf("%s prepare: %w", b.ID, e)
			}
			pass, runErr := runner.Pass(ctx, work, out)
			cleanupErr := cleanup()
			if runErr != nil {
				return s, fmt.Errorf("%s run %d: %w", b.ID, round, runErr)
			}
			if cleanupErr != nil {
				return s, fmt.Errorf("%s cleanup: %w", b.ID, cleanupErr)
			}
			// Every discovered eval must run, including clean-build evals. Skips
			// and absent outcomes cannot support a false-positive score.
			if len(pass.Outcomes) == 0 {
				return s, fmt.Errorf("%s run %d: server eval outcomes missing", b.ID, round)
			}
			for test, outcome := range pass.Outcomes {
				if outcome != "pass" && outcome != "fail" {
					return s, fmt.Errorf("%s run %d: eval %s was not observed (outcome %q)", b.ID, round, test, outcome)
				}
			}
			// A skipped or absent expected test makes the corpus unobservable, not a miss.
			for _, expected := range b.Expected {
				outcome := pass.Outcomes[expected.Test]
				if outcome != "pass" && outcome != "fail" {
					return s, fmt.Errorf("invalid corpus: %s run %d expected test %s was not observed (outcome %q)", b.ID, round, expected.Test, outcome)
				}
			}
			s.Samples = append(s.Samples, Sample{Build: b.ID, Run: round, Pass: pass})
		}
		walls[round-1] = now().Sub(started).Seconds()
	}
	s = score(m, s.Samples)
	// Full wall includes scratch setup, builds, passes, evidence and teardown.
	// Per-lane wall measures the pass alone; neither is a sum of item medians.
	s.RunWallSeconds = walls
	s.WallSeconds = median(walls)
	s.Date = now().UTC().Format(time.RFC3339Nano)
	s.Host, err = os.Hostname()
	if err != nil {
		return s, err
	}
	s.ArgusCommit = commit
	s.OutputDir = opts.Out
	s.GoLOC, err = polymux.GoLOC(opts.Argus)
	if err != nil {
		return s, err
	}
	s.Reason = opts.Reason
	history := filepath.Join(filepath.Dir(filepath.Clean(opts.Corpus)), "history.tsv")
	previous, err := lastHistory(history, s.Host)
	if err != nil {
		return s, err
	}
	s.Regressions = regressions(s, previous)
	if err = writeJSON(filepath.Join(opts.Out, "summary.json"), s); err != nil {
		return s, err
	}
	if err = appendHistory(history, s); err != nil {
		return s, err
	}
	if len(s.Regressions) > 0 && strings.TrimSpace(s.Reason) == "" {
		return s, fmt.Errorf("benchmark regression: %s; supply --reason with the PR explanation", strings.Join(s.Regressions, ", "))
	}
	return s, nil
}

func key(f Finding) string             { return f.Class + "|" + f.Flow + "|" + f.Test }
func median(values [3]float64) float64 { sort.Float64s(values[:]); return values[1] }
func regressions(s Summary, previous *Summary) []string {
	result := []string{}
	if previous == nil {
		return result
	}
	if s.Caught < previous.Caught {
		result = append(result, "bugs caught decreased")
	}
	if s.FalsePositives > previous.FalsePositives {
		result = append(result, "false positives increased")
	}
	if previous.WallSeconds > 0 && s.WallSeconds > previous.WallSeconds*1.10 {
		result = append(result, "full run slowed more than 10%")
	}
	return result
}
func score(m Manifest, samples []Sample) Summary {
	s := Summary{Samples: samples, LaneSeconds: map[string]float64{}, Flaky: []string{}, Missed: []string{}, Regressions: []string{}}
	var costs, calls [3]float64
	lanes := map[string][3]float64{}
	byBuild := map[string][]Sample{}
	resources := map[string]bool{}
	for _, p := range samples {
		for _, resource := range p.Resources {
			resources[resource] = true
		}
		i := p.Run - 1
		costs[i] += p.CostUSD
		calls[i] += float64(p.ModelCalls)
		for lane, seconds := range p.LaneSeconds {
			totals := lanes[lane]
			totals[i] += seconds
			lanes[lane] = totals
		}
		byBuild[p.Build] = append(byBuild[p.Build], p)
	}
	for resource := range resources {
		s.Resources = append(s.Resources, resource)
	}
	sort.Strings(s.Resources)
	s.MovingParts = len(s.Resources)
	s.CostUSD = median(costs)
	s.ModelCalls = int(median(calls))
	for lane, totals := range lanes {
		s.LaneSeconds[lane] = median(totals)
	}
	for _, b := range m.Builds {
		frequency := map[string]int{}
		states := map[string][]string{}
		for _, p := range byBuild[b.ID] {
			found := map[string]bool{}
			for _, f := range p.Findings {
				found[key(f)] = true
			}
			for k := range found {
				frequency[k]++
			}
			for test, result := range p.Outcomes {
				states[test] = append(states[test], result)
			}
		}
		// Only findings caught in all three runs receive credit. Clean false
		// positives use the union, so a flaky false alarm cannot disappear in a median.
		if b.Patch != "" {
			s.Planted++
			caught := true
			for _, f := range b.Expected {
				if frequency[key(f)] != 3 {
					caught = false
					s.Missed = append(s.Missed, b.ID+"|"+key(f))
				}
			}
			if caught {
				s.Caught++
			}
		}
		if b.Patch == "" {
			s.FalsePositives += len(frequency)
		}
		flaky := map[string]bool{}
		for k, n := range frequency {
			if n != 3 {
				flaky[k] = true
			}
		}
		for test, results := range states {
			if len(results) != 3 || results[0] != results[1] || results[0] != results[2] {
				flaky[key(findingFor(test))] = true
			}
		}
		for k := range flaky {
			s.Flaky = append(s.Flaky, b.ID+"|"+k)
		}
	}
	sort.Strings(s.Missed)
	sort.Strings(s.Flaky)
	return s
}
func writeJSON(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(name, append(data, '\n'), 0644)
}

func sortFindings(findings []Finding) {
	sort.Slice(findings, func(i, j int) bool { return key(findings[i]) < key(findings[j]) })
}

// Locate the measured Argus source independently of the invocation directory.
// The explicit override is checked too, preventing accidental Polymux attribution.
func argusRoot(explicit, corpus string) (string, error) {
	start := explicit
	if start == "" {
		start = corpus
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 2 && fields[0] == "module" && fields[1] == "github.com/carlvincetan/argus" {
					return dir, nil
				}
			}
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if explicit != "" || filepath.Dir(dir) == dir {
			break
		}
		dir = filepath.Dir(dir)
	}
	return "", fmt.Errorf("cannot locate Argus source checkout; supply --argus with the Argus module root")
}
