// Package polymux runs the deterministic Polymux server behaviour lane.
package polymux

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Duration is measured in seconds. Gaps describe coverage, not model judgements.
type Item struct {
	ID         string   `json:"id"`
	Lane       string   `json:"lane"`
	LastResult string   `json:"last_result"`
	Duration   float64  `json:"duration"`
	Gaps       []string `json:"gaps"`
	Source     []string `json:"source"`
	tests      []string
}

type Options struct {
	Repo    string
	Out     string
	Changed []string
	Quick   bool     // S1's quick full surface is the entire deterministic server suite.
	Seeded  []string // Explicit PARITY IDs of seeded faults; absent means unknown rate.
}

type Result struct {
	Findings int
	Duration float64
	// Complete distinguishes a completed failing suite from infrastructure failure.
	Complete  bool
	Outcomes  map[string]string
	Resources []string
}

// Executor is injectable so tests exercise real go-test JSON without a server.
type Executor func(context.Context, string, []string, io.Writer) error

func execute(ctx context.Context, dir string, args []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Stdout = out
	// Do not put raw compiler logs or server output in the public summary.
	cmd.Stderr = io.Discard
	return cmd.Run()
}

var evalName = regexp.MustCompile(`^Test([TCBMRGX][1-9][0-9]*)(?:$|[^0-9].*)`)

// ClassifyTest maps the server lane's PARITY test names to finding classes and
// flows. Agent behaviours run in this same server process during B1.
func ClassifyTest(test string) (class, flow string) {
	class, flow = "server_eval", test
	if match := evalName.FindStringSubmatch(test); match != nil {
		flow = match[1]
		switch flow[0] {
		case 'T':
			class = "reply_integrity"
		case 'C':
			class = "tool_contract"
		case 'B':
			class = "delegation"
		case 'M':
			class = "memory"
		case 'R':
			class = "routine"
		case 'G':
			class = "group"
		case 'X':
			class = "recovery"
		}
	}
	return class, flow
}

func catalog(repo string) ([]Item, error) {
	var items []Item
	groups := []struct {
		prefix string
		count  int
		source []string
	}{
		{"T", 5, []string{"apps/server/internal/server/*.go", "apps/server/internal/store/*.go", "apps/server/internal/agentruntime/*.go"}},
		{"C", 6, []string{"apps/server/internal/server/*.go", "apps/server/internal/agentruntime/*.go", "apps/server/internal/store/task.go"}},
		{"B", 5, []string{"apps/server/internal/server/coordination.go", "apps/server/internal/server/proactivity.go", "apps/server/internal/store/*.go"}},
		{"M", 4, []string{"apps/server/internal/memory/*.go", "apps/server/internal/server/memory.go", "apps/server/internal/agentruntime/*.go"}},
		{"R", 4, []string{"apps/server/internal/server/proactivity.go", "apps/server/internal/store/*.go"}},
		{"G", 4, []string{"apps/server/internal/server/*.go", "apps/server/internal/store/run.go", "apps/server/internal/push/*.go"}},
		{"X", 7, []string{"apps/server/internal/server/*.go", "apps/server/internal/store/*.go", "apps/server/internal/agentruntime/*.go"}},
	}
	index := map[string]int{}
	for _, g := range groups {
		for n := 1; n <= g.count; n++ {
			id := g.prefix + strconv.Itoa(n)
			index[id] = len(items)
			items = append(items, Item{ID: id, Lane: "server", LastResult: "not_run", Gaps: []string{}, Source: append([]string{}, g.source...)})
		}
	}
	dir := filepath.Join(repo, "apps", "server", "evals")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read server evals: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, entry.Name()), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse evals: %w", err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			class, flow := ClassifyTest(fn.Name.Name)
			if class == "server_eval" {
				continue
			}
			i, ok := index[flow]
			if !ok {
				i = len(items)
				index[flow] = i
				items = append(items, Item{ID: flow, Lane: "server", LastResult: "not_run", Gaps: []string{}, Source: []string{}})
			}
			items[i].tests = append(items[i].tests, fn.Name.Name)
			source := "apps/server/evals/" + entry.Name()
			if !contains(items[i].Source, source) {
				items[i].Source = append(items[i].Source, source)
			}
		}
	}
	for i := range items {
		if len(items[i].tests) == 0 {
			items[i].Gaps = append(items[i].Gaps, "no eval implemented")
		}
	}
	return items, nil
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func affected(item Item, changed []string, repo string) bool {
	for _, name := range changed {
		if filepath.IsAbs(name) {
			rel, err := filepath.Rel(repo, name)
			if err != nil {
				continue
			}
			name = rel
		}
		name = filepath.ToSlash(filepath.Clean(name))
		for _, glob := range item.Source {
			if match, _ := path.Match(glob, name); match {
				return true
			}
		}
	}
	return false
}

type event struct {
	Action  string
	Test    string
	Package string
	Elapsed float64
}

type outcome struct {
	result   string
	duration float64
}

// parseStream ignores output payloads and subtest completions. The parent test's
// terminal event is authoritative and prevents double-counting failed subtests.
func parseStream(r io.Reader, outcomes map[string]outcome) (bool, bool, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	packageDone, packageFailed := false, false
	for scanner.Scan() {
		var e event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return false, false, fmt.Errorf("invalid go test JSON")
		}
		if e.Action != "pass" && e.Action != "fail" && e.Action != "skip" {
			continue
		}
		if e.Test == "" {
			if packageDone {
				return false, false, fmt.Errorf("unexpected multiple package completions")
			}
			packageDone = true
			if e.Action != "pass" {
				packageFailed = true
			}
			continue
		}
		if strings.Contains(e.Test, "/") {
			continue
		}
		if _, duplicate := outcomes[e.Test]; duplicate {
			return false, false, fmt.Errorf("duplicate test completion")
		}
		outcomes[e.Test] = outcome{e.Action, e.Elapsed}
	}
	return packageDone, packageFailed, scanner.Err()
}

// Run writes coverage and appends a metrics row even when the test command fails.
// A nonzero Findings count is a failed lane; errors mean execution/artifact failure.
func Run(ctx context.Context, opts Options, runner Executor) (Result, error) {
	start := time.Now()
	repo, err := filepath.Abs(opts.Repo)
	if err != nil {
		return Result{}, err
	}
	items, err := catalog(repo)
	if err != nil {
		return Result{}, err
	}
	for _, id := range opts.Seeded {
		found := false
		for _, item := range items {
			if item.ID == id {
				found = true
			}
		}
		if !found {
			return Result{}, fmt.Errorf("unknown seeded PARITY ID %q", id)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return affected(items[i], opts.Changed, repo) && !affected(items[j], opts.Changed, repo)
	})
	if runner == nil {
		runner = execute
	}
	base := []string{"test", "-tags", "sqlite_fts5", "-count=1", "-json", "./evals/"}
	batches := [][]string{base}
	if len(opts.Changed) > 0 {
		selected := map[string]bool{}
		var first, rest []string
		for _, item := range items {
			if affected(item, opts.Changed, repo) {
				for _, name := range item.tests {
					selected[name] = true
					first = append(first, regexp.QuoteMeta(name))
				}
			}
		}
		if len(first) > 0 {
			// Go's RE2 has no negative lookahead. Include every remaining test
			// by name so non-PARITY checks still run in the second batch.
			names, err := allTests(repo)
			if err != nil {
				return Result{}, err
			}
			for _, name := range names {
				if !selected[name] {
					rest = append(rest, regexp.QuoteMeta(name))
				}
			}
			if len(rest) > 0 {
				batches = [][]string{}
				for _, names := range [][]string{first, rest} {
					batches = append(batches, append(append([]string{}, base...), "-run", "^("+strings.Join(names, "|")+")$"))
				}
			}
		}
	}
	outcomes := map[string]outcome{}
	infrastructure := false
	complete := true
	for _, args := range batches {
		var stream bytes.Buffer
		runErr := runner(ctx, filepath.Join(repo, "apps", "server"), args, &stream)
		batch := map[string]outcome{}
		done, failed, parseErr := parseStream(&stream, batch)
		failures := 0
		for name, o := range batch {
			if o.result == "fail" {
				failures++
			}
			if _, duplicate := outcomes[name]; duplicate {
				complete = false
			}
			outcomes[name] = o
		}
		if !done || parseErr != nil || (failed && failures == 0) || (failed != (runErr != nil)) {
			complete = false
		}
		if runErr != nil || !done || failed || parseErr != nil {
			infrastructure = true
		}
	}
	names, err := allTests(repo)
	if err != nil {
		return Result{}, err
	}
	if len(names) == 0 {
		complete = false
	}
	for _, name := range names {
		if _, ok := outcomes[name]; !ok {
			complete = false
		}
	}
	result := Result{Complete: complete, Outcomes: map[string]string{}, Resources: []string{"process:argus", "process:go-test", "host:local"}}
	failedTests := 0
	for name, o := range outcomes {
		result.Outcomes[name] = o.result
		if o.result == "fail" {
			failedTests++
		}
	}
	result.Findings = failedTests
	seeded := map[string]bool{}
	for _, id := range opts.Seeded {
		seeded[id] = true
	}
	caught := 0
	for i := range items {
		item := &items[i]
		if len(item.tests) == 0 {
			continue
		}
		item.LastResult = "pass"
		for _, name := range item.tests {
			o, ok := outcomes[name]
			if !ok {
				item.Gaps = append(item.Gaps, "eval not reached: "+name)
				result.Findings++
				if item.LastResult != "fail" {
					item.LastResult = "not_run"
				}
				continue
			}
			item.Duration += o.duration
			switch o.result {
			case "fail":
				item.LastResult = "fail"
			case "skip":
				item.Gaps = append(item.Gaps, "eval skipped: "+name)
				result.Findings++
				if item.LastResult == "pass" {
					item.LastResult = "skip"
				}
			}
		}
		if item.LastResult == "fail" && seeded[item.ID] {
			caught++
		}
	}
	if !complete || len(outcomes) == 0 || infrastructure {
		// A package-only build failure, malformed/truncated stream or startup error
		// must fail even if no individual test reported a failure.
		if failedTests == 0 {
			result.Findings++
			items = append(items, Item{ID: "server", Lane: "server", LastResult: "fail", Gaps: []string{"go test did not complete successfully"}, Source: []string{"apps/server/evals/*.go"}})
		}
	}
	loc, err := GoLOC(repo)
	if err != nil {
		return result, err
	}
	result.Duration = time.Since(start).Seconds()
	if err := os.MkdirAll(opts.Out, 0755); err != nil {
		return result, err
	}
	coverage, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return result, err
	}
	if err := os.WriteFile(filepath.Join(opts.Out, "coverage.json"), append(coverage, '\n'), 0644); err != nil {
		return result, err
	}
	rate := ""
	if len(seeded) > 0 {
		rate = strconv.FormatFloat(float64(caught)/float64(len(seeded)), 'f', 6, 64)
	}
	// Two processes (Argus and go test), no models, one local host.
	movingParts := len(result.Resources)
	metrics, err := os.OpenFile(filepath.Join(opts.Out, "metrics.tsv"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return result, err
	}
	defer metrics.Close()
	info, err := metrics.Stat()
	if err != nil {
		return result, err
	}
	if info.Size() == 0 {
		if _, err := fmt.Fprintln(metrics, "full_surface_s\tsimplicity\tcatch_rate\tgo_loc\tmoving_parts"); err != nil {
			return result, err
		}
	}
	_, err = fmt.Fprintf(metrics, "%.6f\t%d\t%s\t%d\t%d\n", result.Duration, loc+movingParts, rate, loc, movingParts)
	return result, err
}

func allTests(repo string) ([]string, error) {
	files, err := parser.ParseDir(token.NewFileSet(), filepath.Join(repo, "apps", "server", "evals"), func(info fs.FileInfo) bool { return strings.HasSuffix(info.Name(), "_test.go") }, 0)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, pkg := range files {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") && fn.Name.Name != "TestMain" {
					names = append(names, fn.Name.Name)
				}
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

// Count physical Go source lines, including tests; generated/vendor/build output
// is excluded so simplicity measures maintained repository code.
func GoLOC(repo string) (int, error) {
	total := 0
	err := filepath.WalkDir(repo, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "node_modules", ".build", "var":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("Code generated")) && bytes.Contains(data, []byte("DO NOT EDIT.")) {
			return nil
		}
		total += bytes.Count(data, []byte{'\n'})
		if len(data) > 0 && data[len(data)-1] != '\n' {
			total++
		}
		return nil
	})
	return total, err
}
