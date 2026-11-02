package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/carlvincetan/argus/internal/intent"
)

const intentUsage = `usage:
  argus intent list    --repo DIR    exits 1 if any intent is stale
  argus intent add     --repo DIR --id ID --check CHECK --element PATTERN --reason WHY --paths FILES [--journey ID]
  argus intent confirm --repo DIR --id ID
  argus intent remove  --repo DIR --id ID
`

// intentCommand maintains the ledger of behaviour that is meant, kept in the app's
// repository so that it changes in the same commit as the code it describes.
func intentCommand(args []string, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "argus intent: %v\n", err)
		return 1
	}
	if len(args) == 0 {
		fmt.Fprint(stderr, intentUsage)
		return 2
	}
	fs := flag.NewFlagSet("intent "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "app repository (required)")
	id := fs.String("id", "", "intent id")
	check := fs.String("check", "", "sensor check it covers")
	element := fs.String("element", "", "element pattern, where * matches anything")
	journey := fs.String("journey", "", "limit it to one journey")
	reason := fs.String("reason", "", "why this is meant")
	var paths []string
	fs.Var(stringSliceFlag{target: &paths}, "paths", "source files that make it true, comma-separated or repeatable")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return fail(err)
	}
	if *repo == "" {
		return fail(errors.New("--repo is required"))
	}
	ledger, err := intent.Load(*repo)
	if err != nil {
		return fail(err)
	}
	var change error
	switch args[0] {
	case "list":
		stale := 0
		for _, entry := range ledger.Intents {
			status := "ok"
			if ledger.Stale(entry.ID) {
				status = "STALE"
				stale++
			}
			fmt.Fprintf(stdout, "%-5s %s  %s %s\n      %s (confirmed %s)\n", status, entry.ID, entry.Check, entry.Element, entry.Reason, entry.Confirmed)
		}
		if stale > 0 {
			fmt.Fprintf(stdout, "%d stale: confirm each if it still holds, or remove it\n", stale)
			return 1
		}
		return 0
	case "add":
		change = ledger.Add(intent.Intent{ID: *id, Check: *check, Element: *element, Journey: *journey, Reason: *reason, Paths: splitList(paths)})
	case "confirm":
		change = ledger.Confirm(*id)
	case "remove":
		change = ledger.Remove(*id)
	default:
		fmt.Fprint(stderr, intentUsage)
		return 2
	}
	if change != nil {
		return fail(change)
	}
	if err := ledger.Save(); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "%s: %s\n", args[0], *id)
	return 0
}
