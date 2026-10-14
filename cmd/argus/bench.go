package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/carlvincetan/argus/internal/bench"
)

func benchCommand(args []string) error {
	const usage = "usage: argus bench polymux --corpus benchmark/polymux --polymux CHECKOUT --out DIR [--argus CHECKOUT] [--reason TEXT] [--pass-timeout DURATION]"
	if len(args) == 0 {
		return errors.New(usage)
	}
	if args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(os.Stdout, usage)
		return nil
	}
	if args[0] != "polymux" {
		return fmt.Errorf("unknown benchmark %q", args[0])
	}
	fs := flag.NewFlagSet("bench polymux", flag.ContinueOnError)
	corpus := fs.String("corpus", "benchmark/polymux", "versioned corpus directory")
	repo := fs.String("polymux", "", "local Polymux checkout (required)")
	out := fs.String("out", "var/bench/"+time.Now().Format("20060102-150405"), "result directory (repeat invocations retain earlier evidence)")
	argus := fs.String("argus", "", "Argus source checkout (default: locate from corpus)")
	reason := fs.String("reason", "", "stated reason for a benchmark regression")
	timeout := fs.Duration("pass-timeout", bench.DefaultPassTimeout, "maximum time per build pass; timeout retains evidence and aborts scoring")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *repo == "" || fs.NArg() != 0 {
		return fmt.Errorf("--polymux CHECKOUT is required; unexpected positional arguments are not allowed")
	}
	if *timeout <= 0 {
		return fmt.Errorf("--pass-timeout must be positive")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := bench.Run(ctx, bench.Options{Corpus: *corpus, Polymux: *repo, Out: *out, Argus: *argus, Reason: *reason, PassTimeout: *timeout}, nil)
	if err != nil {
		return err
	}
	fmt.Printf("ARGUS BENCH: %d/%d caught, %d false positives, %d missed findings, %d flaky items, median %.3f s\n", result.Caught, result.Planted, result.FalsePositives, len(result.Missed), len(result.Flaky), result.WallSeconds)
	fmt.Printf("Evidence: %s\n", result.OutputDir)
	return nil
}
