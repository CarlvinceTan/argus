// Command argus is an autonomous UI defect hunter.
//
// Usage:
//
//	argus run --subject NAME --cmd "program args" [flags]
//	argus canary [--model-url URL] [--profile subject|eval]
//	argus version
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/carlvincetan/argus/internal/agent"
	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/harness"
	"github.com/carlvincetan/argus/internal/isolate"
	"github.com/carlvincetan/argus/internal/provider"
	"github.com/carlvincetan/argus/internal/report"
	"github.com/carlvincetan/argus/internal/run"
	"github.com/carlvincetan/argus/internal/triage"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "bench":
		if err := benchCommand(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "argus: %v\n", err)
			os.Exit(1)
		}
	case "polymux":
		os.Exit(polymuxCommand(os.Args[2:], os.Stdout, os.Stderr))
	case "run":
		if err := runCommand(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "argus: %v\n", err)
			os.Exit(1)
		}
	case "canary":
		if err := canaryCommand(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "argus: %v\n", err)
			os.Exit(1)
		}
	case "tui-rows":
		if err := tuiRowsCommand(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "argus: %v\n", err)
			os.Exit(1)
		}
	case "x11probe":
		if err := x11ProbeCommand(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "argus: %v\n", err)
			os.Exit(1)
		}
	case "triage":
		if err := triageCommand(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "argus: %v\n", err)
			os.Exit(1)
		}
	case "version", "--version", "-v":
		fmt.Printf("argus %s\n", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "argus: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `argus: autonomous UI defect hunter

  argus run     --subject NAME --cmd "program args" [flags]
  argus canary  [--model-url URL] [--profile subject|eval]
  argus triage  label|list|stats|calibrate|reset
  argus bench polymux --corpus benchmark/polymux --polymux CHECKOUT --out DIR
  argus polymux --repo DIR [--changed FILES] [--quick] --out DIR
  argus version

triage:
  argus triage label --run DIR --finding ID --verdict accepted|rejected|misclassified|intentional|duplicate [--note TEXT]
  argus triage list [--limit N] [--verdict V]
  argus triage stats
  argus triage calibrate [--min-samples N]
  argus triage reset --yes

Labelling is how the system learns: each verdict also stores the finding's
features, and 'argus triage calibrate' turns them into data the pipeline reads
(var/triage/calibration.json). Deleting that file restores the stock behaviour.

run flags:
  --cols, --rows        terminal size (default 100x30)
  --steps               maximum exploration steps
  --out                 artefact directory (default var/runs/<subject>-<time>)
  --arg                 extra subject argument (repeatable)
  --version, --commit   subject version information recorded in the report
  --workdir             working directory for the subject
  --env KEY=VALUE       environment variable for the subject (repeatable)
  --vision              enable the vision tier (default true)
  --video               write video evidence (default true)
  --subject-online      do not sandbox the subject (network allowed)
  --offline             require a loopback-only local model configuration
  --canary              run sandbox network probes and record the evidence
  --sandbox-self        run the whole evaluation under the loopback-only sandbox
  --quiet               only print findings
  --provider            openrouter (Jev) | commandcode (chat model) | local (loopback)
  --mode                tui (default), gui (X11) or web (browser)
  --url                 web mode: page to audit, for example http://127.0.0.1:8000/
  --browser             web mode: Chrome/Chromium binary (default: auto-detect)
  --headless            web mode: run headless (default true)
  --display             gui mode: X11 display, e.g. localhost:99
  --x11-agent           gui mode: host:port of an argus-x11-agent on the GUI host
  --window              gui mode: WM_NAME substring to find the subject window
  --clicks              gui mode: include clicks in the action space (default true)
  --text-input          allow typing into input fields (off by default)
  --persona             reviewer to run (repeatable); default: visual, behaviour, completeness
`)
}

func runCommand(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	subject := fs.String("subject", "", "name of the program under test")
	cmd := fs.String("cmd", "", "command line to run")
	cols := fs.Int("cols", 100, "terminal columns")
	rows := fs.Int("rows", 30, "terminal rows")
	steps := fs.Int("steps", 0, "maximum exploration steps")
	out := fs.String("out", "", "artefact directory")
	versionFlag := fs.String("version", "", "subject version string")
	commitFlag := fs.String("commit", "", "subject commit")
	workdir := fs.String("workdir", "", "subject working directory")
	vision := fs.Bool("vision", true, "enable the vision tier")
	video := fs.Bool("video", true, "write video evidence")
	subjectOnline := fs.Bool("subject-online", false, "do not sandbox the subject")
	noSandbox := fs.Bool("no-sandbox", false, "alias for --subject-online")
	offline := fs.Bool("offline", false, "require loopback-only local model configuration")
	canary := fs.Bool("canary", false, "run sandbox network probes")
	sandboxSelf := fs.Bool("sandbox-self", false, "re-run this process inside the loopback-only sandbox")
	quiet := fs.Bool("quiet", false, "only print findings")
	textInput := fs.Bool("text-input", false, "allow the explorer to type into input fields (changes program state)")
	var personaFlags []string
	fs.Var(stringSliceFlag{target: &personaFlags}, "persona", "specialist reviewer to run (repeatable): visual, behaviour, animation, completeness, errors")
	mode := fs.String("mode", "tui", "subject kind: tui, gui or web")
	pageURL := fs.String("url", "", "web mode: the page to audit")
	browser := fs.String("browser", "", "web mode: path to a Chrome/Chromium binary (default: auto-detect)")
	headless := fs.Bool("headless", true, "web mode: run the browser headless")
	display := fs.String("display", "", "X11 display for gui mode (for example localhost:99)")
	x11Agent := fs.String("x11-agent", "", "gui mode: address of an argus-x11-agent running next to the subject")
	windowMatch := fs.String("window", "", "gui mode: WM_NAME substring of the subject window")
	allowClicks := fs.Bool("clicks", true, "gui mode: allow clicks in the action space")
	providerFlag := fs.String("provider", "", "reasoning provider: openrouter (TypeSafe Jev), commandcode, or local")
	var extra []string
	fs.Var(stringSliceFlag{target: &extra}, "arg", "extra subject argument")
	envFlags := map[string]string{}
	fs.Var(kvFlag{target: envFlags}, "env", "environment variable for the subject (KEY=VALUE, repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	isWebMode := strings.EqualFold(*mode, "web")
	if *subject == "" {
		return fmt.Errorf("--subject is required")
	}
	if *cmd == "" && !isWebMode {
		return fmt.Errorf("--cmd is required unless --mode web")
	}
	if isWebMode && *pageURL == "" {
		return fmt.Errorf("--url is required with --mode web")
	}

	cfg := config.Default()
	if err := cfg.Load(); err != nil {
		return err
	}
	if *providerFlag != "" {
		cfg.Provider = strings.ToLower(strings.TrimSpace(*providerFlag))
		cfg.ApplyProviderDefaults()
	}
	if *sandboxSelf {
		// Re-exec under the loopback-only profile: after this point neither the
		// analyser nor the subject can reach the internet, which is what makes
		// an offline evaluation claim checkable rather than asserted.
		if err := reexecSandboxed(cfg, os.Args[1:]); err != nil {
			return err
		}
		return nil
	}
	if *offline {
		if err := cfg.AssertOfflineReady(); err != nil {
			return err
		}
	}
	if *subjectOnline || *noSandbox {
		cfg.SubjectOffline = false
	}
	insideEvalSandbox := os.Getenv("ARGUS_SANDBOXED") == "1"
	if insideEvalSandbox {
		// The outer evaluation sandbox already covers the subject: a nested
		// sandbox-exec is not permitted on macOS.
		cfg.SubjectOffline = false
	}
	if *steps > 0 {
		cfg.Limits.MaxSteps = *steps
	}

	argv := append(splitCommand(*cmd), extra...)
	providers, err := provider.Build(cfg, *vision)
	if err != nil {
		return err
	}

	runDir := *out
	if runDir == "" {
		runDir = filepath.Join(cfg.RunDir, fmt.Sprintf("%s-%s", slug(*subject), time.Now().Format("20060102-150405")))
	}
	writer, err := report.NewWriter(runDir, *video)
	if err != nil {
		return err
	}

	isolation := map[string]any{}
	if insideEvalSandbox {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		selfEvidence := isolate.Probe(ctx, cfg.LocalBaseURL)
		cancel()
		isolation["self_canary"] = selfEvidence
		isolation["self_sandboxed"] = true
		if !*quiet {
			fmt.Printf("self canary: passed=%t (no internet from the evaluation process)\n", selfEvidence.Passed)
		}
	}
	profilePath := ""
	if cfg.SubjectOffline {
		profile, err := isolate.SubjectProfile(filepath.Join(runDir, "sandbox"))
		if err != nil {
			return err
		}
		profilePath = profile.Path
		isolation["subject_profile_hash"] = profile.Hash
		isolation["sandbox_available"] = isolate.SandboxAvailable()
		if *canary {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			evidence := isolate.Canary(ctx, profile, "")
			cancel()
			isolation["subject_canary"] = evidence
			if !*quiet {
				fmt.Printf("canary: passed=%t\n", evidence.Passed)
				for _, result := range evidence.Results {
					fmt.Printf("  %-22s expected=%-7s exit=%d passed=%t\n", result.Name, result.Expected, result.ExitCode, result.Passed)
				}
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	observer := func(format string, args ...any) {
		if !*quiet {
			fmt.Printf("argus: "+format+"\n", args...)
		}
	}
	subjectInfo := run.Subject{
		Name:     *subject,
		Argv:     argv,
		Version:  *versionFlag,
		Commit:   *commitFlag,
		WorkDir:  *workdir,
		Terminal: fmt.Sprintf("%dx%d", *cols, *rows),
	}
	if strings.EqualFold(*mode, "web") {
		if *pageURL == "" {
			return fmt.Errorf("web mode needs --url")
		}
		var browserArgs []string
		if !*headless {
			browserArgs = append(browserArgs, "--headless=false")
		}
		webOutcome, err := agent.RunGUISession(ctx, cfg, providers, agent.GUIRunOptions{
			Subject:     subjectInfo,
			URL:         *pageURL,
			Browser:     *browser,
			Cols:        *cols,
			Rows:        *rows,
			MaxSteps:    cfg.Limits.MaxSteps,
			UseVision:   *vision,
			Personas:    personaFlags,
			Observer:    observer,
			Isolation:   isolation,
			AllowClicks: true,
		})
		if err != nil {
			return err
		}
		writer.IndexImages(webOutcome.Frames)
		if err := writer.WriteSession(webOutcome.Session); err != nil {
			return err
		}
		fmt.Printf("\nfindings (%d reported of %d):\n", countReported(webOutcome.Session), len(webOutcome.Session.Findings))
		for _, finding := range webOutcome.Session.Findings {
			if !finding.Reported {
				continue
			}
			fmt.Printf("  [%-10s] %-44s %s (%s)\n", finding.SeverityName, truncate(finding.Title, 44), finding.Screen, finding.Group)
		}
		fmt.Printf("\nrun: %s\nreport: %s\ncost: $%.5f in %s\n",
			webOutcome.Session.ID, filepath.Join(runDir, "report.md"), webOutcome.Session.Cost.CostUSD,
			webOutcome.Session.Duration.Round(time.Millisecond))
		return nil
	}
	if strings.EqualFold(*mode, "gui") {
		guiOutcome, err := agent.RunGUISession(ctx, cfg, providers, agent.GUIRunOptions{
			Subject:        subjectInfo,
			Display:        *display,
			AgentAddr:      *x11Agent,
			WindowMatch:    *windowMatch,
			MaxSteps:       cfg.Limits.MaxSteps,
			SandboxProfile: profilePath,
			UseVision:      *vision,
			AllowClicks:    *allowClicks,
			Observer:       observer,
			Isolation:      isolation,
		})
		if err != nil {
			return err
		}
		writer.IndexImages(guiOutcome.Frames)
		if err := writer.WriteSession(guiOutcome.Session); err != nil {
			return err
		}
		fmt.Printf("\nfindings (%d reported of %d):\n", countReported(guiOutcome.Session), len(guiOutcome.Session.Findings))
		for _, finding := range guiOutcome.Session.Findings {
			if !finding.Reported {
				continue
			}
			fmt.Printf("  [%-10s] %-40s %s (%s)\n", finding.SeverityName, truncate(finding.Title, 40), finding.Screen, finding.Group)
		}
		fmt.Printf("\nrun: %s\nreport: %s\ncost: $%.5f in %s\n",
			guiOutcome.Session.ID, filepath.Join(runDir, "report.md"), guiOutcome.Session.Cost.CostUSD,
			guiOutcome.Session.Duration.Round(time.Millisecond))
		return nil
	}
	outcome, err := agent.RunSession(ctx, cfg, providers, agent.RunOptions{
		Subject:        subjectInfo,
		Cols:           *cols,
		Rows:           *rows,
		MaxSteps:       cfg.Limits.MaxSteps,
		SandboxProfile: profilePath,
		UseVision:      *vision,
		AllowTextInput: *textInput,
		Personas:       personaFlags,
		Observer:       observer,
		Isolation:      isolation,
	})
	if err != nil {
		return err
	}
	writer.Index(outcome.Grids)
	if err := writer.WriteSession(outcome.Session); err != nil {
		return err
	}

	session := outcome.Session
	if !*quiet {
		fmt.Printf("\nfindings (%d reported of %d):\n", countReported(session), len(session.Findings))
	}
	for _, finding := range session.Findings {
		if !finding.Reported {
			continue
		}
		fmt.Printf("  [%-10s] %-40s %s (%s)\n", finding.SeverityName, truncate(finding.Title, 40), finding.Screen, finding.Group)
	}
	fmt.Printf("\nrun: %s\nreport: %s\ncost: $%.5f in %s\n",
		session.ID, filepath.Join(runDir, "report.md"), session.Cost.CostUSD, session.Duration.Round(time.Millisecond))
	return nil
}

// reexecSandboxed replaces this process with itself running under the
// loopback-only sandbox profile. ARGUS_SANDBOXED marks the child so it does not
// recurse, and the profile is written next to the run artefacts.
func reexecSandboxed(cfg config.Config, args []string) error {
	if os.Getenv("ARGUS_SANDBOXED") == "1" {
		return nil
	}
	if !isolate.SandboxAvailable() {
		return fmt.Errorf("--sandbox-self needs /usr/bin/sandbox-exec (macOS)")
	}
	profileDir := filepath.Join(cfg.RunDir, "sandbox")
	profile, err := isolate.EvalProfile(profileDir, modelPort(cfg.LocalBaseURL))
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the argus binary: %w", err)
	}
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--sandbox-self" {
			continue
		}
		filtered = append(filtered, arg)
	}
	command := exec.Command("/usr/bin/sandbox-exec", append([]string{"-f", profile.Path, "--", self}, filtered...)...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.Env = append(os.Environ(), "ARGUS_SANDBOXED=1")
	return command.Run()
}

// modelPort extracts the port from a loopback base URL for the sandbox profile.
func modelPort(baseURL string) int {
	trimmed := strings.TrimRight(baseURL, "/")
	index := strings.LastIndex(trimmed, ":")
	if index < 0 {
		return 80
	}
	port := 0
	for _, char := range trimmed[index+1:] {
		if char < '0' || char > '9' {
			break
		}
		port = port*10 + int(char-'0')
	}
	if port == 0 {
		return 80
	}
	return port
}

// tuiRowsCommand prints the captured terminal rows as Go string literals, which
// is how the check thresholds are calibrated against real screens.
func tuiRowsCommand(args []string) error {
	fs := flag.NewFlagSet("tui-rows", flag.ExitOnError)
	command := fs.String("cmd", "", "command to run")
	cols := fs.Int("cols", 64, "columns")
	rows := fs.Int("rows", 14, "rows")
	seconds := fs.Int("seconds", 2, "how long to observe")
	keys := fs.String("keys", "", "keys to send before capturing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *command == "" {
		return fmt.Errorf("--cmd is required")
	}
	h, err := harness.Start(harness.Options{Argv: splitCommand(*command), Cols: *cols, Rows: *rows})
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	h.WaitIdle(300*time.Millisecond, 3*time.Second)
	if *keys != "" {
		if err := h.SendText(*keys); err != nil {
			return err
		}
	}
	time.Sleep(time.Duration(*seconds) * time.Second)
	grid := h.Snapshot()
	for index, row := range grid.Rows {
		if strings.TrimSpace(row) == "" {
			continue
		}
		fmt.Printf("%2d %q\n", index, row)
	}
	return nil
}

// x11ProbeCommand connects to an X display, reports what it can see, and writes
// a screenshot so a GUI setup can be checked without running a full session.
func x11ProbeCommand(args []string) error {
	fs := flag.NewFlagSet("x11probe", flag.ExitOnError)
	display := fs.String("display", ":99", "X11 display")
	agent := fs.String("x11-agent", "", "address of an X11 agent to probe instead of a local display")
	windowMatch := fs.String("window", "", "WM_NAME substring")
	seconds := fs.Int("seconds", 3, "how long to observe")
	out := fs.String("out", "var/x11probe.png", "where to write a screenshot")
	inputs := fs.Bool("inputs", false, "send a few keys after observing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	probe, err := harness.OpenGUI(*agent, *display, *windowMatch)
	if err != nil {
		return err
	}
	defer func() { _ = probe.Close() }()
	width, height := probe.Size()
	fmt.Printf("display %s: window %dx%d\n", *display, width, height)
	deadline := time.Now().Add(time.Duration(*seconds) * time.Second)
	frames := 0
	changedTotal := 0
	slowest := time.Duration(0)
	for time.Now().Before(deadline) {
		started := time.Now()
		frame, err := probe.Capture()
		if err != nil {
			return err
		}
		if elapsed := time.Since(started); elapsed > slowest {
			slowest = elapsed
		}
		frames++
		changedTotal += frame.ChangedPixels
		time.Sleep(120 * time.Millisecond)
	}
	fmt.Printf("slowest capture: %s\n", slowest.Round(time.Millisecond))
	ledger := probe.Frames()
	fmt.Printf("captures=%d ledger=%d total changed pixels=%d\n", frames, len(ledger), changedTotal)
	if len(ledger) > 0 {
		last := ledger[len(ledger)-1]
		if err := os.WriteFile(*out, last.PNG, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%dx%d, %d bytes)\n", *out, last.Width, last.Height, len(last.PNG))
		if last.Image != nil {
			colours := map[uint32]int{}
			bounds := last.Image.Bounds()
			for y := bounds.Min.Y; y < bounds.Max.Y; y += 3 {
				for x := bounds.Min.X; x < bounds.Max.X; x += 3 {
					c := last.Image.RGBAAt(x, y)
					colours[uint32(c.R)<<16|uint32(c.G)<<8|uint32(c.B)]++
				}
			}
			dominant := 0
			for _, count := range colours {
				if count > dominant {
					dominant = count
				}
			}
			sampled := (bounds.Dx()/3 + 1) * (bounds.Dy()/3 + 1)
			fmt.Printf("distinct colours: %d, dominant colour covers %.0f%% of the window\n",
				len(colours), 100*float64(dominant)/float64(sampled))
		}
	}
	if *agent != "" {
		windows, err := harness.RemoteWindows(*agent)
		if err == nil {
			for _, window := range windows {
				fmt.Printf("window id=%d name=%q %dx%d mapped=%t\n", window.ID, window.Name, window.Width, window.Height, window.Mapped)
			}
		}
	}
	if *inputs {
		for _, key := range []string{"2", "3", "d", "DOWN"} {
			if err := probe.SendKey(key); err != nil {
				fmt.Printf("key %s failed: %v\n", key, err)
			}
			time.Sleep(200 * time.Millisecond)
			if _, err := probe.Capture(); err != nil {
				fmt.Printf("capture after %s failed: %v\n", key, err)
			}
		}
		fmt.Printf("after input: ledger=%d\n", len(probe.Frames()))
	}
	return nil
}

// triageCommand records reviewer verdicts and derives calibration data.
func triageCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: argus triage label|list|stats|calibrate|reset")
	}
	cfg := config.Default()
	if err := cfg.Load(); err != nil {
		return err
	}
	store, err := triage.Open(cfg.LabelPath)
	if err != nil {
		return err
	}
	switch args[0] {
	case "label":
		fs := flag.NewFlagSet("triage label", flag.ExitOnError)
		runPath := fs.String("run", "", "run directory or run.json")
		finding := fs.String("finding", "", "finding id, or a substring of its title")
		verdict := fs.String("verdict", "", "accepted|rejected|misclassified|intentional|duplicate")
		note := fs.String("note", "", "free-text reason (this is the valuable part)")
		reviewer := fs.String("reviewer", os.Getenv("USER"), "who is labelling")
		all := fs.Bool("all", false, "label every reported finding in the run")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *runPath == "" || *verdict == "" {
			return fmt.Errorf("--run and --verdict are required")
		}
		if !triage.Valid(*verdict) {
			return fmt.Errorf("unknown verdict %q (use: accepted, rejected, misclassified, intentional, duplicate)", *verdict)
		}
		session, err := readRun(*runPath)
		if err != nil {
			return err
		}
		targets, err := selectFindings(session, *finding, *all)
		if err != nil {
			return err
		}
		for _, target := range targets {
			label := labelFromFinding(session, target, triage.Verdict(*verdict), *note, *reviewer)
			if err := store.Append(label); err != nil {
				return err
			}
			fmt.Printf("labelled %s as %s: %s\n", target.ID, *verdict, target.Title)
		}
		fmt.Printf("%d label(s) written to %s\n", len(targets), store.Path())

	case "list":
		fs := flag.NewFlagSet("triage list", flag.ExitOnError)
		limit := fs.Int("limit", 20, "how many labels to print")
		verdict := fs.String("verdict", "", "only this verdict")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		labels, err := store.All()
		if err != nil {
			return err
		}
		printed := 0
		for index := len(labels) - 1; index >= 0 && printed < *limit; index-- {
			label := labels[index]
			if *verdict != "" && string(label.Verdict) != *verdict {
				continue
			}
			fmt.Printf("%-14s %-38s %-14s %s\n", label.Verdict, truncate(label.Title, 38), label.Check, label.Note)
			printed++
		}
		if printed == 0 {
			fmt.Println("no labels yet")
		}

	case "stats":
		labels, err := store.All()
		if err != nil {
			return err
		}
		report := triage.Summarise(labels)
		encoded, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(encoded))

	case "calibrate":
		fs := flag.NewFlagSet("triage calibrate", flag.ExitOnError)
		minSamples := fs.Int("min-samples", 0, "labels needed before a check can be suppressed")
		out := fs.String("out", cfg.CalibrationPath, "where to write the calibration")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		options := triage.DefaultOptions()
		if *minSamples > 0 {
			options.MinSamples = *minSamples
		}
		labels, err := store.All()
		if err != nil {
			return err
		}
		calibration := triage.Build(labels, options)
		if err := calibration.Save(*out); err != nil {
			return err
		}
		fmt.Printf("calibration from %d labels: %s\nwritten to %s\n", len(labels), calibration.Summary(), *out)

	case "reset":
		fs := flag.NewFlagSet("triage reset", flag.ExitOnError)
		confirm := fs.Bool("yes", false, "confirm deletion")
		labelsOnly := fs.Bool("labels", false, "delete only the label log")
		calibrationOnly := fs.Bool("calibration", false, "delete only the calibration")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if !*confirm {
			return fmt.Errorf("pass --yes to confirm (this deletes the learning data)")
		}
		targets := []string{cfg.LabelPath, cfg.CalibrationPath}
		if *labelsOnly {
			targets = []string{cfg.LabelPath}
		}
		if *calibrationOnly {
			targets = []string{cfg.CalibrationPath}
		}
		for _, target := range targets {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			fmt.Printf("removed %s\n", target)
		}

	default:
		return fmt.Errorf("unknown triage command %q", args[0])
	}
	return nil
}

func readRun(path string) (*run.Session, error) {
	if !strings.HasSuffix(path, "run.json") {
		path = filepath.Join(path, "run.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var session run.Session
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &session, nil
}

func selectFindings(session *run.Session, idOrTitle string, all bool) ([]run.Finding, error) {
	if all {
		reported := []run.Finding{}
		for _, finding := range session.Findings {
			if finding.Reported {
				reported = append(reported, finding)
			}
		}
		if len(reported) == 0 {
			return nil, fmt.Errorf("this run has no reported findings")
		}
		return reported, nil
	}
	if idOrTitle == "" {
		return nil, fmt.Errorf("--finding is required unless --all is passed")
	}
	needle := strings.ToLower(idOrTitle)
	for _, finding := range session.Findings {
		if finding.ID == idOrTitle || strings.Contains(strings.ToLower(finding.Title), needle) {
			return []run.Finding{finding}, nil
		}
	}
	return nil, fmt.Errorf("no finding in this run matches %q", idOrTitle)
}

func labelFromFinding(
	session *run.Session,
	finding run.Finding,
	verdict triage.Verdict,
	note string,
	reviewer string,
) triage.Label {
	return triage.Label{
		FindingID:    finding.ID,
		RunID:        session.ID,
		Subject:      session.Subject.Name,
		Kind:         session.Kind,
		Check:        finding.Check,
		Class:        finding.Class,
		Group:        finding.Group,
		Severity:     finding.SeverityName,
		Verified:     finding.Verified,
		Title:        finding.Title,
		DefectProb:   finding.DefectProb,
		ModelAnswers: finding.ModelAnswers,
		Verdict:      verdict,
		Note:         note,
		Reviewer:     reviewer,
		At:           time.Now().UTC(),
	}
}

func canaryCommand(args []string) error {
	fs := flag.NewFlagSet("canary", flag.ExitOnError)
	modelURL := fs.String("model-url", "http://127.0.0.1:8080/v1", "local model URL")
	profileKind := fs.String("profile", "subject", "subject or eval")
	out := fs.String("out", filepath.Join("var", "runs", "canary"), "where to write the profile")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var profile isolate.Profile
	var err error
	switch *profileKind {
	case "eval":
		profile, err = isolate.EvalProfile(*out, 8080)
	default:
		profile, err = isolate.SubjectProfile(*out)
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	evidence := isolate.Canary(ctx, profile, *modelURL)
	encoded, _ := json.MarshalIndent(evidence, "", "  ")
	fmt.Println(string(encoded))
	if !evidence.Passed {
		os.Exit(1)
	}
	return nil
}

type kvFlag struct {
	target map[string]string
}

func (k kvFlag) String() string {
	parts := make([]string, 0, len(k.target))
	for name, value := range k.target {
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, " ")
}

func (k kvFlag) Set(value string) error {
	name, val, found := strings.Cut(value, "=")
	if !found || strings.TrimSpace(name) == "" {
		return fmt.Errorf("expected KEY=VALUE, got %q", value)
	}
	k.target[strings.TrimSpace(name)] = val
	return nil
}

type stringSliceFlag struct {
	target *[]string
}

func (s stringSliceFlag) String() string {
	if s.target == nil {
		return ""
	}
	return strings.Join(*s.target, " ")
}

func (s stringSliceFlag) Set(value string) error {
	*s.target = append(*s.target, value)
	return nil
}

// splitCommand splits a command line on spaces, honouring simple quotes.
func splitCommand(line string) []string {
	out := []string{}
	current := strings.Builder{}
	quote := rune(0)
	for _, char := range line {
		switch {
		case quote != 0:
			if char == quote {
				quote = 0
				continue
			}
			current.WriteRune(char)
		case char == '"' || char == '\'':
			quote = char
		case char == ' ' || char == '\t':
			if current.Len() > 0 {
				out = append(out, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(char)
		}
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}

func countReported(session *run.Session) int {
	count := 0
	for _, finding := range session.Findings {
		if finding.Reported {
			count++
		}
	}
	return count
}

func slug(text string) string {
	lower := strings.ToLower(strings.TrimSpace(text))
	builder := strings.Builder{}
	for _, char := range lower {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			builder.WriteRune(char)
		case char == ' ' || char == '_' || char == '/':
			builder.WriteRune('-')
		}
	}
	if builder.Len() == 0 {
		return "subject"
	}
	return builder.String()
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}
