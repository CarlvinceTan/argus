package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/carlvincetan/argus/internal/agent"
	"github.com/carlvincetan/argus/internal/harness"
	"github.com/carlvincetan/argus/internal/intent"
	"github.com/carlvincetan/argus/internal/sensors"
	"github.com/carlvincetan/argus/internal/swarm"
)

const (
	baselineDir   = "e2e/baselines"
	defaultImages = "var/baselines"
)

const baselineUsage = `usage:
  argus baseline list    --repo DIR
  argus baseline approve --repo DIR --run DIR --reason WHY (--journey ID ... | --all --confirm DIR) [--images DIR]

--confirm names a second run. A journey is only approved if both runs agree, because
one recording can catch a moment the app doesn't reliably reach. --all requires it.
`

func baselinePath(repo, journey string) string {
	return filepath.Join(repo, baselineDir, journey+".json")
}

// saveShots keeps each state's screenshot with the run, for a later approve.
func saveShots(dir string, shots map[int]*harness.X11Frame) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for index, frame := range shots {
		if frame == nil || len(frame.PNG) == 0 {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.png", index)), frame.PNG, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// baselineCommand is the only thing that writes a baseline. A run never does, so a
// change can't pass by being observed; someone has to say why it's wanted.
func baselineCommand(args []string, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "argus baseline: %v\n", err)
		return 1
	}
	if len(args) == 0 {
		fmt.Fprint(stderr, baselineUsage)
		return 2
	}
	fs := flag.NewFlagSet("baseline "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "app repository (required)")
	runDir := fs.String("run", "", "run directory to approve from")
	confirm := fs.String("confirm", "", "second run that must have reached the same states")
	reason := fs.String("reason", "", "why this is the approved state")
	images := fs.String("images", defaultImages, "directory of approved screenshots")
	all := fs.Bool("all", false, "approve every journey in the run")
	var journeys []string
	fs.Var(stringSliceFlag{target: &journeys}, "journey", "journey to approve, comma-separated or repeatable")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return fail(err)
	}
	if *repo == "" {
		return fail(errors.New("--repo is required"))
	}
	switch args[0] {
	case "list":
		return baselineList(*repo, stdout, fail)
	case "approve":
		if err := baselineApprove(*repo, *runDir, *confirm, *reason, *images, *all, splitList(journeys), stdout); err != nil {
			return fail(err)
		}
		return 0
	}
	fmt.Fprint(stderr, baselineUsage)
	return 2
}

func baselineList(repo string, stdout io.Writer, fail func(error) int) int {
	paths, err := filepath.Glob(filepath.Join(repo, baselineDir, "*.json"))
	if err != nil {
		return fail(err)
	}
	sort.Strings(paths)
	for _, path := range paths {
		trace, err := agent.LoadReplay(path)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "%-36s %2d steps, approved %s: %s\n",
			strings.TrimSuffix(filepath.Base(path), ".json"), len(trace.Steps), trace.Approved, trace.Reason)
	}
	return 0
}

func baselineApprove(repo, runDir, confirm, reason, images string, all bool, journeys []string, stdout io.Writer) error {
	if runDir == "" || strings.TrimSpace(reason) == "" {
		return errors.New("--run and --reason are required: a baseline records why it was accepted")
	}
	if all == (len(journeys) > 0) {
		return errors.New("name each --journey, or pass --all")
	}
	if all && confirm == "" {
		return errors.New("--all needs --confirm: a broad approval has to reproduce in a second run")
	}
	ledger, err := intent.Load(repo)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(runDir, "swarm.json"))
	if err != nil {
		return err
	}
	var report swarm.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		return fmt.Errorf("reading %s: %w", runDir, err)
	}
	refused := []string{}
	wanted := map[string]bool{}
	for _, journey := range journeys {
		wanted[journey] = true
	}
	for _, result := range report.Results {
		id := result.Journey.ID
		if !all && !wanted[id] {
			continue
		}
		delete(wanted, id)
		refusal := unfit(result)
		if refusal == "" && confirm != "" {
			refusal = reproduced(runDir, confirm, id, ledger)
		}
		if refusal != "" {
			// One bad journey shouldn't block the rest of a broad approval, but it can't
			// pass unnoticed either: the command still fails at the end.
			if !all {
				return errors.New(refusal)
			}
			refused = append(refused, refusal)
			continue
		}
		summary, err := approveJourney(repo, runDir, id, reason, images)
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		fmt.Fprintf(stdout, "approved %s: %s\n", id, summary)
	}
	for journey := range wanted {
		return fmt.Errorf("%s isn't in that run", journey)
	}
	if len(refused) > 0 {
		return fmt.Errorf("%d journeys were not approved:\n  %s", len(refused), strings.Join(refused, "\n  "))
	}
	return nil
}

// reproduced says where two runs of a journey disagree, or nothing if they agree.
// Known variation is left out, so that it can't block every approval.
func reproduced(runDir, confirm, id string, ledger *intent.Ledger) string {
	first, err := agent.LoadReplay(filepath.Join(runDir, id, "trace.json"))
	if err != nil || first == nil {
		return fmt.Sprintf("%s can't be approved: its trace is unreadable", id)
	}
	second, err := agent.LoadReplay(filepath.Join(confirm, id, "trace.json"))
	if err != nil || second == nil {
		return fmt.Sprintf("%s can't be approved: the second run has no trace for it", id)
	}
	if len(first.Steps) != len(second.Steps) {
		return fmt.Sprintf("%s can't be approved: the runs took %d and %d steps", id, len(first.Steps), len(second.Steps))
	}
	disagree := func(want, got *sensors.State, step string) string {
		if want == nil || got == nil {
			return ""
		}
		for _, item := range sensors.Drift(*want, *got, 1, step) {
			if _, known := ledger.Match(item.Check, item.Snippet, id); !known {
				return fmt.Sprintf("%s can't be approved: the runs disagree: %s", id, item.Detail)
			}
		}
		return ""
	}
	if refusal := disagree(first.State, second.State, "startup"); refusal != "" {
		return refusal
	}
	for index := range first.Steps {
		step := first.Steps[index]
		if refusal := disagree(step.State, second.Steps[index].State, step.Action.Label); refusal != "" {
			return refusal
		}
	}
	return ""
}

// unfit says why a run can't be trusted as a baseline.
func unfit(result swarm.Result) string {
	id := result.Journey.ID
	switch {
	case result.Error != "":
		return fmt.Sprintf("%s can't be approved: its run failed: %s", id, result.Error)
	case result.Crashed:
		return fmt.Sprintf("%s can't be approved: the app crashed in that run", id)
	case result.Blind > 0:
		return fmt.Sprintf("%s can't be approved: %d steps have no screenshot", id, result.Blind)
	}
	return ""
}

func approveJourney(repo, runDir, id, reason, images string) (string, error) {
	trace, err := agent.LoadReplay(filepath.Join(runDir, id, "trace.json"))
	if err != nil {
		return "", err
	}
	if trace == nil || trace.State == nil {
		return "", errors.New("the run recorded no states")
	}
	path := baselinePath(repo, id)
	summary := "new baseline"
	old, err := agent.LoadReplay(path)
	if err != nil {
		return "", err
	}
	if old != nil {
		summary = summarise(old, trace)
	}
	trace.Approved, trace.Reason = time.Now().Format("2006-01-02"), reason
	if err := trace.Save(path); err != nil {
		return "", err
	}
	target := filepath.Join(images, id)
	if err := os.RemoveAll(target); err != nil {
		return "", err
	}
	return summary, copyShots(filepath.Join(runDir, id, "states"), target)
}

// summarise says how much the approval changes, so that a broad approve shows.
func summarise(old, next *agent.Replay) string {
	added, gone, moved := 0, 0, 0
	count := func(want, got *sensors.State) {
		if want == nil || got == nil {
			return
		}
		for _, item := range sensors.Drift(*want, *got, 1, "") {
			switch item.Check {
			case sensors.DriftAdded:
				added++
			case sensors.DriftGone:
				gone++
			default:
				moved++
			}
		}
	}
	count(old.State, next.State)
	for index := range min(len(old.Steps), len(next.Steps)) {
		count(old.Steps[index].State, next.Steps[index].State)
	}
	return fmt.Sprintf("%d added, %d gone, %d moved", added, gone, moved)
}

func copyShots(from, to string) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(from, entry.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(to, entry.Name()), raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}
