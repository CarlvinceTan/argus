package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/carlvincetan/argus/internal/polymux"
)

func polymuxCommand(args []string, stdout, stderr io.Writer) int {
	start := time.Now()
	fs := flag.NewFlagSet("polymux", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "Polymux checkout (required)")
	out := fs.String("out", "", "coverage and metrics directory (required)")
	quick := fs.Bool("quick", false, "quick full surface: deterministic server evals only")
	var changed, seeded []string
	fs.Var(stringSliceFlag{target: &changed}, "changed", "changed paths, comma-separated or repeatable; matching evals run first")
	fs.Var(stringSliceFlag{target: &seeded}, "seeded", "seeded PARITY IDs, comma-separated or repeatable (optional catch-rate denominator)")
	fail := func(err error) int {
		fmt.Fprintf(stderr, "argus polymux: %v\n", err)
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
	if fs.NArg() > 0 {
		return fail(errors.New("unexpected positional arguments; use --changed for paths"))
	}
	split := func(values []string) []string {
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := polymux.Run(ctx, polymux.Options{Repo: *repo, Out: *out, Quick: *quick, Changed: split(changed), Seeded: split(seeded)}, nil)
	if err != nil {
		return fail(err)
	}
	status, code := "PASS", 0
	if result.Findings > 0 {
		status, code = "FAIL", 1
	}
	fmt.Fprintf(stdout, "ARGUS: %s %d findings, %.3fs\n", status, result.Findings, result.Duration)
	return code
}
