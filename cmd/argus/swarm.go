package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/carlvincetan/argus/internal/agent"
	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/harness"
	"github.com/carlvincetan/argus/internal/intent"
	"github.com/carlvincetan/argus/internal/polymux"
	"github.com/carlvincetan/argus/internal/provider"
	"github.com/carlvincetan/argus/internal/report"
	"github.com/carlvincetan/argus/internal/run"
	"github.com/carlvincetan/argus/internal/sensors"
	"github.com/carlvincetan/argus/internal/swarm"
)

// avoidLabels are controls with effects outside the app: a dictation button
// raises a system prompt and an open panel blocks the run.
var avoidLabels = []string{"dictation", "speech", "attachment", "microphone", "choose file"}

const (
	controlAttempts = 3
	controlBackoff  = 5 * time.Second
)

type uiOptions struct {
	repo     string
	app      string
	control  string
	steps    int
	replay   string
	images   string
	fresh    bool
	intents  *intent.Ledger
	cfg      config.Config
	set      provider.Set
	progress func(format string, args ...any)
}

// polymuxUICommand runs journeys against isolated Polymux instances.
func polymuxUICommand(args []string, stdout, stderr io.Writer) int {
	start := time.Now()
	fs := flag.NewFlagSet("polymux ui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "Polymux checkout (required)")
	control := fs.String("control", "", "checkout whose granted Control helper to use (default: --repo)")
	out := fs.String("out", "", "evidence directory (required)")
	limit := fs.Int("journeys", 0, "most journeys to run (default: all)")
	parallel := fs.Int("parallel", 3, "journeys to run at once")
	steps := fs.Int("steps", 0, "actions per journey")
	shard := fs.String("shard", "", "run one CI shard, for example 2/4")
	providerName := fs.String("provider", "none", "reasoning provider; none reports measurements only")
	replay := fs.String("replay", "", "directory of traces from passing runs, reused and updated")
	images := fs.String("images", defaultImages, "directory of approved baseline screenshots")
	fresh := fs.Bool("no-baseline", false, "explore instead of comparing with approved baselines")
	var changed []string
	fs.Var(stringSliceFlag{target: &changed}, "changed", "changed paths, comma-separated or repeatable; default is the whole surface")
	since := fs.String("changed-since", "", "plan from what changed since the merge base with this ref, for example origin/main")
	filterName := fs.String("filter", "", "decision provider that drops journeys the change can't affect (needs --changed-since); default: area rules only")
	dryRun := fs.Bool("dry-run", false, "print the journeys that would run, and why others were left out, then stop")
	issues := fs.String("issues", "", "issue ledger to compare with: issues it holds open don't fail the run")
	record := fs.Bool("record", false, "fold this run into --issues (main or nightly runs only: a rerun must not make a regression known)")
	fail := func(err error) int {
		fmt.Fprintf(stderr, "argus polymux ui: %v\n", err)
		fmt.Fprintf(stdout, "ARGUS: FAIL 1 findings, %.3fs\n", time.Since(start).Seconds())
		return 1
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return fail(err)
	}
	if *repo == "" || *out == "" {
		return fail(errors.New("--repo and --out are required"))
	}
	root, err := filepath.Abs(*repo)
	if err != nil {
		return fail(err)
	}
	if *since != "" && len(changed) > 0 {
		return fail(errors.New("use --changed or --changed-since, not both"))
	}
	if *record && *issues == "" {
		return fail(errors.New("--record needs --issues"))
	}
	if *filterName != "" && *since == "" {
		return fail(errors.New("--filter needs --changed-since: the model judges the diff, not file names"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := config.Default()
	if err := cfg.Load(); err != nil {
		return fail(err)
	}
	filterCfg := cfg
	cfg.Provider = strings.ToLower(strings.TrimSpace(*providerName))
	cfg.ApplyProviderDefaults()
	set, err := provider.Build(cfg, cfg.Provider != "none")
	if err != nil {
		return fail(err)
	}
	var change polymux.Change
	if *since != "" {
		if change, err = polymux.ChangesSince(ctx, root, *since); err != nil {
			return fail(err)
		}
		changed = change.Paths() // nothing changed plans the whole surface
	}
	planLimit := *limit
	if *filterName != "" {
		planLimit = 0 // the limit applies to what the filter keeps
	}
	journeys, err := polymux.PlanJourneys(root, splitList(changed), planLimit)
	if err != nil {
		return fail(err)
	}
	var selection polymux.Selection
	if *filterName != "" {
		filterCfg.Provider = strings.ToLower(strings.TrimSpace(*filterName))
		filterCfg.ApplyProviderDefaults()
		filterSet, err := provider.Build(filterCfg, false)
		if err != nil {
			return fail(err)
		}
		if filterSet.Decisions == nil {
			return fail(fmt.Errorf("--filter %s has no decision model", *filterName))
		}
		selection = polymux.DecideJourneys(ctx, root, journeys, change, filterSet.Decisions)
		journeys = selection.Kept
		if selection.Note != "" {
			fmt.Fprintf(stderr, "argus: %s\n", selection.Note)
		}
		if *limit > 0 && len(journeys) > *limit {
			journeys = journeys[:*limit]
		}
	}
	if *shard != "" {
		index, total, err := parseShard(*shard)
		if err != nil {
			return fail(err)
		}
		journeys = polymux.Shard(journeys, index, total)
	}
	if *dryRun {
		printPlan(stdout, journeys, selection)
		return 0
	}
	if len(journeys) == 0 {
		fmt.Fprintf(stdout, "ARGUS: PASS 0 findings, %.3fs\n", time.Since(start).Seconds())
		return 0
	}
	ledger, err := intent.Load(root)
	if err != nil {
		return fail(err)
	}
	load, cores, err := polymux.Doctor()
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stderr, "argus: %d journeys, %d at once\n", len(journeys), *parallel)
	options := uiOptions{
		repo: root, control: root, steps: *steps, replay: *replay, images: *images, fresh: *fresh,
		intents: ledger, cfg: cfg, set: set,
	}
	if *control != "" {
		options.control = *control
	}
	options.progress = func(format string, args ...any) { fmt.Fprintf(stderr, "argus: "+format+"\n", args...) }
	build := filepath.Join(*out, "build")
	options.app, err = polymux.BuildApp(ctx, root, build)
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(options.app)
	result := swarm.Run(ctx, journeys, *parallel, *out, func(ctx context.Context, journey polymux.Journey, dir string) swarm.Result {
		return runJourney(ctx, options, journey, dir)
	})
	result.Load, result.Cores = load, cores
	result.Selection, result.LeftOut = selection.Note, selection.LeftOut
	for _, entry := range ledger.Intents {
		switch {
		case ledger.Stale(entry.ID):
			result.Stale = append(result.Stale, entry.ID)
		case result.Honoured[entry.ID] == 0 && len(splitList(changed)) == 0 && *limit == 0 && *shard == "":
			result.Unused = append(result.Unused, entry.ID)
		}
	}
	blocking := result.Blocking()
	if *issues != "" {
		issueLedger, err := swarm.LoadLedger(*issues)
		if err != nil {
			return fail(err)
		}
		issueLedger.Compare(&result)
		if known := blocking - result.BlockingNew(); known > 0 {
			fmt.Fprintf(stderr, "argus: %d blocking issues are already open in %s and don't fail this run\n", known, *issues)
		}
		blocking = result.BlockingNew()
		if *record {
			issueLedger.Record(result, time.Now())
			if err := issueLedger.Save(*issues); err != nil {
				return fail(err)
			}
		}
	}
	if err := result.Write(*out); err != nil {
		return fail(err)
	}
	findings := blocking + result.Failed()
	status, code := "PASS", 0
	if findings > 0 {
		status, code = "FAIL", 1
	}
	fmt.Fprintf(stdout, "ARGUS: %s %d findings, %.3fs\n", status, findings, time.Since(start).Seconds())
	return code
}

// printPlan lists what a run would do, like `e2e list --changed-since`.
func printPlan(stdout io.Writer, journeys []polymux.Journey, selection polymux.Selection) {
	if selection.Note != "" {
		fmt.Fprintln(stdout, selection.Note)
	}
	fmt.Fprintf(stdout, "%d journeys would run:\n", len(journeys))
	for _, journey := range journeys {
		fmt.Fprintf(stdout, "  %s (%s)\n", journey.ID, strings.Join(journey.Flows, ", "))
		if journey.Linked != "" {
			fmt.Fprintf(stdout, "    added: its baseline shows %s\n", journey.Linked)
		}
	}
	if len(selection.LeftOut) > 0 {
		left := make([]string, 0, len(selection.LeftOut))
		for id := range selection.LeftOut {
			left = append(left, id)
		}
		sort.Strings(left)
		fmt.Fprintf(stdout, "%d left out by the decision filter:\n", len(left))
		for _, id := range left {
			fmt.Fprintf(stdout, "  %s: %s\n", id, selection.LeftOut[id])
		}
	}
}

func splitList(values []string) []string {
	var result []string
	for _, value := range values {
		for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' }) {
			if part = strings.TrimSpace(part); part != "" {
				result = append(result, part)
			}
		}
	}
	return result
}

func parseShard(value string) (int, int, error) {
	first, second, ok := strings.Cut(value, "/")
	index, err := strconv.Atoi(first)
	total, err2 := strconv.Atoi(second)
	if !ok || err != nil || err2 != nil || total < 1 || index < 1 || index > total {
		return 0, 0, fmt.Errorf("--shard %q: want INDEX/TOTAL, for example 2/4", value)
	}
	return index, total, nil
}

// openControl retries because a fresh fixture launch can briefly have no window,
// and macOS can briefly report Control's permissions as missing.
func openControl(repo, instance string) (*harness.ControlHarness, error) {
	var last error
	for attempt := 0; attempt < controlAttempts; attempt++ {
		opened, err := harness.StartControl(harness.ControlOptions{Repo: repo, Instance: instance})
		if err == nil {
			return opened, nil
		}
		last = err
		time.Sleep(controlBackoff)
	}
	return nil, last
}

// runJourney owns one instance from start to stop, so nothing outlives it.
func runJourney(ctx context.Context, options uiOptions, journey polymux.Journey, dir string) swarm.Result {
	failed := func(err error) swarm.Result { return swarm.Result{Error: err.Error()} }
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return failed(err)
	}
	progress := func(format string, args ...any) { options.progress(journey.ID+": "+format, args...) }
	var previous *agent.Replay
	trace := ""
	if options.replay != "" {
		trace = filepath.Join(options.replay, journey.ID+".json")
		loaded, err := agent.LoadReplay(trace)
		if err != nil {
			return failed(err)
		}
		previous = loaded
	}
	baselined := false
	if !options.fresh {
		approved, err := agent.LoadReplay(baselinePath(options.repo, journey.ID))
		if err != nil {
			return failed(err)
		}
		// A baseline outranks a trace, which updates itself and so can't guard anything.
		if approved != nil {
			previous, baselined = approved, true
			trace = ""
		}
	}
	instance, err := polymux.StartInstance(ctx, options.repo, options.app, journey.Flags, filepath.Join(dir, "instance.log"))
	if err != nil {
		return failed(err)
	}
	defer func() {
		if err := instance.Stop(); err != nil {
			progress("%v", err)
		}
	}()
	progress("running in %s", filepath.Base(instance.Dir))
	opened, err := openControl(options.control, instance.Dir)
	if err != nil {
		return failed(err)
	}
	defer opened.Close()
	outcome, err := agent.RunNativeSession(ctx, options.cfg, options.set, agent.NativeOptions{
		Subject:  run.Subject{Name: journey.ID, Argv: journey.Flags, WorkDir: options.repo, Tags: journey.Flows},
		Harness:  opened,
		MaxSteps: options.steps,
		Avoid:    avoidLabels,
		Replay:   previous,
		Baseline: baselined,
		Images:   filepath.Join(options.images, journey.ID),
		Intents:  options.intents,
		Observer: progress,
		Diagnostics: func() []sensors.Evidence {
			return polymux.ScanLog(filepath.Join(instance.Dir, "app.log"))
		},
	})
	if err != nil {
		return failed(err)
	}
	writer, err := report.NewWriter(dir, false)
	if err != nil {
		return failed(err)
	}
	writer.IndexImages(outcome.Frames)
	if err := writer.WriteSession(outcome.Session); err != nil {
		return failed(err)
	}
	if err := outcome.Trace.Save(filepath.Join(dir, "trace.json")); err != nil {
		return failed(err)
	}
	if err := saveShots(filepath.Join(dir, "states"), outcome.Shots); err != nil {
		return failed(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "states.mmd"), []byte(outcome.Trace.Mermaid()), 0o644); err != nil {
		return failed(err)
	}
	// The instance and its logs are deleted on stop; evidence has to outlive it.
	if raw, err := os.ReadFile(filepath.Join(instance.Dir, "app.log")); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "app.log"), raw, 0o644)
	}
	if trace != "" && !outcome.Crashed {
		if err := outcome.Trace.Save(trace); err != nil {
			return failed(err)
		}
	}
	result := swarm.Result{
		Session: outcome.Session, Findings: countReported(outcome.Session), Crashed: outcome.Crashed,
		Replayed: outcome.Replayed, Drifted: outcome.Drifted, Blind: outcome.Blind, Perf: outcome.Perf,
		Compared: outcome.Compared,
		Calls:    outcome.Session.Cost.DecisionCalls,
	}
	// A run with no screenshot at all saw too little to call the app clean.
	if len(outcome.Frames) == 0 {
		result.Error = "no screenshot succeeded: " + strings.Join(outcome.Session.Notes, "; ")
	}
	if outcome.Unfinished != "" {
		result.Error = outcome.Unfinished
	}
	return result
}
