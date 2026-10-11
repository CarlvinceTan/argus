// Command argus-bench runs the benchmark subjects and scores the findings
// against curated ground truth.
//
// The ground truth lives in benchmark/subjects.json, which is never passed to
// the models: a run receives only the subject command, a terminal size and a
// step budget. Scoring matches reported findings against the curated signature
// for each defect (class or keyword), which keeps the comparison mechanical
// instead of vibes-based.
//
//	argus-bench run   --argus ./var/argus --subjects benchmark/subjects.json --out benchmark/results
//	argus-bench score --subjects benchmark/subjects.json --results benchmark/results
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/carlvincetan/argus/internal/run"
)

// Subject is one benchmark case with a pre-fix and an optional post-fix binary.
type Subject struct {
	ID          string      `json:"id"`
	Title       string      `json:"title"`
	Why         string      `json:"why"`
	Probe       string      `json:"probe"`
	Steps       int         `json:"steps"`
	Cols        int         `json:"cols"`
	Rows        int         `json:"rows"`
	Pre         *Variant    `json:"pre"`
	Post        *Variant    `json:"post"`
	GroundTruth []TruthItem `json:"ground_truth"`
}

// Variant is one build of a subject.
type Variant struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Argv    []string `json:"argv"`
	WorkDir string   `json:"workdir,omitempty"`
	Env     []string `json:"env,omitempty"`
}

// TruthItem is a curated expected defect. It is written in the evaluator's own
// words (never copied from a patch), and matched by defect class or keyword.
type TruthItem struct {
	ID         string   `json:"id"`
	Summary    string   `json:"summary"`
	Classes    []string `json:"classes"`
	Keywords   []string `json:"keywords"`
	ExpectPre  string   `json:"expect_pre"`
	ExpectPost string   `json:"expect_post"`
}

type scorecard struct {
	Subjects []subjectScore `json:"subjects"`
	Summary  summary        `json:"summary"`
}

type subjectScore struct {
	ID     string        `json:"id"`
	Title  string        `json:"title"`
	Pre    variantScore  `json:"pre"`
	Post   *variantScore `json:"post,omitempty"`
	Truth  []truthResult `json:"truth"`
	Missed []string      `json:"missed_truth"`
	Weak   []string      `json:"weak_matches"`
	Noisy  []string      `json:"noise"`
}

type variantScore struct {
	Name     string  `json:"name"`
	RunDir   string  `json:"run_dir"`
	RunID    string  `json:"run_id"`
	Reported int     `json:"reported_findings"`
	Total    int     `json:"total_candidates"`
	Cost     float64 `json:"cost_usd"`
	Duration string  `json:"duration"`
}

type truthResult struct {
	ID       string `json:"id"`
	Matched  bool   `json:"matched"`
	Weak     bool   `json:"weak,omitempty"`
	Finding  string `json:"finding,omitempty"`
	Severity string `json:"severity,omitempty"`
	Expect   string `json:"expect"`
}

type summary struct {
	Subjects        int     `json:"subjects"`
	TruthItems      int     `json:"truth_items"`
	DetectedOnPre   int     `json:"detected_on_pre"`
	QuietOnPost     int     `json:"quiet_on_post"`
	PostTruthItems  int     `json:"post_truth_items"`
	ReportedOnPost  int     `json:"reported_on_post"`
	PrecisionOnPost float64 `json:"precision_on_post"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: argus-bench run|score [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		if err := runBench(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "argus-bench: %v\n", err)
			os.Exit(1)
		}
	case "score":
		if err := scoreBench(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "argus-bench: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "argus-bench: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func runBench(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	argusBin := fs.String("argus", "./var/argus", "path to the argus binary")
	subjectsPath := fs.String("subjects", "benchmark/subjects.json", "subject definitions")
	outDir := fs.String("out", "benchmark/results", "result directory")
	provider := fs.String("provider", "openrouter", "reasoning provider for the runs")
	extra := fs.String("extra", "", "extra argus flags")
	only := fs.String("only", "", "run only subjects whose id contains this text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	subjects, err := loadSubjects(*subjectsPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	for _, subject := range subjects {
		if *only != "" && !strings.Contains(subject.ID, *only) {
			continue
		}
		for _, variant := range []*Variant{subject.Pre, subject.Post} {
			if variant == nil {
				continue
			}
			runDir := filepath.Join(*outDir, fmt.Sprintf("%s-%s", subject.ID, sanitise(variant.Version)))
			// The subject argv is passed positionally: joining it into one
			// --cmd string loses quoting and shell operators (a pipeline in a
			// subject command is normal, and silently dropping it tests the
			// wrong program).
			command := []string{
				*argusBin, "run",
				"--subject", subject.ID,
				"--cmd", variant.Argv[0],
				"--version", variant.Version,
				"--cols", fmt.Sprintf("%d", subject.Cols),
				"--rows", fmt.Sprintf("%d", subject.Rows),
				"--steps", fmt.Sprintf("%d", subject.Steps),
				"--provider", *provider,
				"--quiet",
				"--out", runDir,
			}
			for _, arg := range variant.Argv[1:] {
				command = append(command, "--arg="+arg)
			}
			if variant.WorkDir != "" {
				command = append(command, "--workdir", variant.WorkDir)
			}
			for _, entry := range variant.Env {
				command = append(command, "--env="+entry)
			}
			if *extra != "" {
				command = append(command, strings.Fields(*extra)...)
			}
			fmt.Printf("==> %s (%s)\n", subject.ID, variant.Name)
			started := time.Now()
			runner := exec.Command(command[0], command[1:]...)
			runner.Stdout = os.Stdout
			runner.Stderr = os.Stderr
			if err := runner.Run(); err != nil {
				fmt.Printf("    (run reported an error: %v)\n", err)
			}
			fmt.Printf("    finished in %s -> %s\n", time.Since(started).Round(time.Second), runDir)
		}
	}
	return nil
}

func scoreBench(args []string) error {
	fs := flag.NewFlagSet("score", flag.ExitOnError)
	subjectsPath := fs.String("subjects", "benchmark/subjects.json", "subject definitions")
	resultsDir := fs.String("results", "benchmark/results", "result directory")
	outPath := fs.String("out", "", "write the scorecard JSON here")
	if err := fs.Parse(args); err != nil {
		return err
	}
	subjects, err := loadSubjects(*subjectsPath)
	if err != nil {
		return err
	}
	card := scorecard{Subjects: []subjectScore{}}
	for _, subject := range subjects {
		score := subjectScore{ID: subject.ID, Title: subject.Title, Truth: []truthResult{}, Missed: []string{}, Weak: []string{}, Noisy: []string{}}
		pre, err := loadSession(*resultsDir, subject, subject.Pre)
		if err != nil {
			fmt.Printf("%s: pre run missing (%v)\n", subject.ID, err)
			continue
		}
		score.Pre = summariseVariant(subject.Pre, *resultsDir, pre)
		post, postErr := loadSession(*resultsDir, subject, subject.Post)
		if postErr == nil && post != nil {
			summary := summariseVariant(subject.Post, *resultsDir, post)
			score.Post = &summary
		}

		reported := reportedFindings(pre)
		for _, truth := range subject.GroundTruth {
			result := truthResult{ID: truth.ID, Expect: truth.ExpectPre}
			finding, ok, weak := matchFinding(reported, truth)
			switch {
			case ok:
				result.Matched = true
				result.Finding = finding.Title
				result.Severity = finding.SeverityName
			case weak:
				result.Weak = true
				result.Finding = finding.Title
				result.Severity = finding.SeverityName
				score.Weak = append(score.Weak, truth.ID)
			default:
				score.Missed = append(score.Missed, truth.ID)
			}
			score.Truth = append(score.Truth, result)
		}
		if post != nil {
			for _, finding := range reportedFindings(post) {
				if truth, ok := matchAny(subject.GroundTruth, finding); ok && truth.ExpectPost == "quiet" {
					score.Noisy = append(score.Noisy, fmt.Sprintf("%s: %s", truth.ID, finding.Title))
				}
			}
			card.Summary.PostTruthItems += len(subject.GroundTruth)
			card.Summary.ReportedOnPost += len(reportedFindings(post))
		}
		card.Summary.Subjects++
		card.Summary.TruthItems += len(subject.GroundTruth)
		for _, result := range score.Truth {
			if result.Matched {
				card.Summary.DetectedOnPre++
			}
		}
		card.Subjects = append(card.Subjects, score)
	}
	if card.Summary.PostTruthItems > 0 {
		card.Summary.PrecisionOnPost = 1 - float64(card.Summary.ReportedOnPost)/float64(card.Summary.PostTruthItems)
		if card.Summary.PrecisionOnPost < 0 {
			card.Summary.PrecisionOnPost = 0
		}
	}

	printScorecard(card)
	if *outPath != "" {
		encoded, err := json.MarshalIndent(card, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*outPath, encoded, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func printScorecard(card scorecard) {
	fmt.Printf("\n%-38s %-10s %-10s %-10s\n", "SUBJECT", "DETECTED", "MISSED", "NOISE(POST)")
	for _, subject := range card.Subjects {
		detected := 0
		for _, result := range subject.Truth {
			if result.Matched {
				detected++
			}
		}
		fmt.Printf("%-38s %-10s %-10d %-10d\n", subject.ID, fmt.Sprintf("%d/%d", detected, len(subject.Truth)), len(subject.Missed), len(subject.Noisy))
		for _, result := range subject.Truth {
			mark := "MISS"
			switch {
			case result.Matched:
				mark = "OK"
			case result.Weak:
				mark = "WEAK"
			}
			fmt.Printf("    [%s] %-22s %s %s\n", mark, result.ID, result.Finding, result.Severity)
		}
		for _, noisy := range subject.Noisy {
			fmt.Printf("    [noise] %s\n", noisy)
		}
	}
	fmt.Printf("\ntruth detected on pre-fix: %d/%d (class+keyword); findings reported on post-fix: %d\n",
		card.Summary.DetectedOnPre, card.Summary.TruthItems, card.Summary.ReportedOnPost)
}

func loadSubjects(path string) ([]Subject, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading subjects: %w", err)
	}
	var subjects []Subject
	if err := json.Unmarshal(raw, &subjects); err != nil {
		return nil, fmt.Errorf("parsing subjects: %w", err)
	}
	return subjects, nil
}

func loadSession(resultsDir string, subject Subject, variant *Variant) (*run.Session, error) {
	if variant == nil {
		return nil, fmt.Errorf("no variant")
	}
	path := filepath.Join(resultsDir, fmt.Sprintf("%s-%s", subject.ID, sanitise(variant.Version)), "run.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var session run.Session
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

func summariseVariant(variant *Variant, resultsDir string, session *run.Session) variantScore {
	reported := 0
	for _, finding := range session.Findings {
		if finding.Reported {
			reported++
		}
	}
	return variantScore{
		Name:     variant.Name,
		RunDir:   filepath.Join(resultsDir, session.ID),
		RunID:    session.ID,
		Reported: reported,
		Total:    len(session.Findings),
		Cost:     session.Cost.CostUSD,
		Duration: session.Duration.Round(time.Millisecond).String(),
	}
}

func reportedFindings(session *run.Session) []run.Finding {
	out := []run.Finding{}
	for _, finding := range session.Findings {
		if finding.Reported {
			out = append(out, finding)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Severity > out[j].Severity })
	return out
}

// matchFinding matches one curated expectation against the reported findings.
// A detection requires the defect class *and* a keyword from the curated
// signature; a class-only match is reported separately as weak so the scorecard
// cannot be inflated by a vague finding of the right category.
func matchFinding(findings []run.Finding, truth TruthItem) (run.Finding, bool, bool) {
	for _, finding := range findings {
		if classMatch(finding.Class, truth.Classes) && keywordMatch(finding, truth.Keywords) {
			return finding, true, false
		}
	}
	for _, finding := range findings {
		if classMatch(finding.Class, truth.Classes) {
			return finding, false, true
		}
	}
	return run.Finding{}, false, false
}

func matchAny(truths []TruthItem, finding run.Finding) (TruthItem, bool) {
	for _, truth := range truths {
		if classMatch(finding.Class, truth.Classes) && keywordMatch(finding, truth.Keywords) {
			return truth, true
		}
	}
	return TruthItem{}, false
}

func classMatch(class string, classes []string) bool {
	for _, candidate := range classes {
		if class == candidate {
			return true
		}
	}
	return false
}

func keywordMatch(finding run.Finding, keywords []string) bool {
	text := strings.ToLower(finding.Title + " " + finding.Brief + " " + finding.Detail + " " + finding.Screen)
	for _, keyword := range keywords {
		if strings.Contains(text, strings.ToLower(keyword)) {
			return true
		}
	}
	return false
}

func sanitise(text string) string {
	replaced := strings.NewReplacer("/", "-", " ", "-", ":", "-").Replace(text)
	return strings.Trim(replaced, "-")
}
