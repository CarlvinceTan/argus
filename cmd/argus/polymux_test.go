package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolymuxHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := polymuxCommand([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("help exit %d", code)
	}
	for _, flag := range []string{"-repo", "-changed", "-quick", "-out"} {
		if !strings.Contains(stderr.String(), flag) {
			t.Errorf("help missing %s", flag)
		}
	}
	if strings.Contains(stderr.String(), "panic") {
		t.Fatalf("help panic: %s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("unexpected summary %q", stdout.String())
	}
}

func TestPolymuxInvalidFlagsFailSummary(t *testing.T) {
	for _, args := range [][]string{nil, {"--unknown"}, {"--repo", "/missing", "--out", t.TempDir()}} {
		var stdout, stderr bytes.Buffer
		if code := polymuxCommand(args, &stdout, &stderr); code != 1 {
			t.Fatalf("exit %d", code)
		}
		if !strings.HasPrefix(stdout.String(), "ARGUS: FAIL 1 findings, ") || strings.Count(stdout.String(), "\n") != 1 {
			t.Fatalf("summary %q", stdout.String())
		}
	}
}

// Exercise the command's real subprocess path with a fake go-test JSON stream.
func TestPolymuxCommandSummaryAndExit(t *testing.T) {
	for _, status := range []string{"pass", "fail"} {
		t.Run(status, func(t *testing.T) {
			dir := t.TempDir()
			repo := filepath.Join(dir, "repo")
			evals := filepath.Join(repo, "apps/server/evals")
			if err := os.MkdirAll(evals, 0755); err != nil {
				t.Fatal(err)
			}
			source := "package evals\nimport \"testing\"\nfunc TestT2EndTurn(t *testing.T) {}\n"
			if err := os.WriteFile(filepath.Join(evals, "scenarios_test.go"), []byte(source), 0644); err != nil {
				t.Fatal(err)
			}
			exit := "0"
			if status == "fail" {
				exit = "1"
			}
			script := "#!/bin/sh\n[ \"$*\" = 'test -tags sqlite_fts5 -count=1 -json ./evals/' ] || exit 2\n" +
				"[ \"$PWD\" = \"$ARGUS_TEST_SERVER_DIR\" ] || exit 2\n" +
				"printf '%s\\n' '{\"Action\":\"" + status + "\",\"Test\":\"TestT2EndTurn\",\"Elapsed\":0.2}' '{\"Action\":\"" + status + "\",\"Elapsed\":0.2}'\nexit " + exit + "\n"
			if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			t.Setenv("ARGUS_TEST_SERVER_DIR", filepath.Join(repo, "apps/server"))
			var stdout, stderr bytes.Buffer
			code := polymuxCommand([]string{"--repo", repo, "--quick", "--out", filepath.Join(dir, "out")}, &stdout, &stderr)
			wantCode, wantSummary := 0, "ARGUS: PASS 0 findings, "
			if status == "fail" {
				wantCode, wantSummary = 1, "ARGUS: FAIL 1 findings, "
			}
			if code != wantCode || !strings.HasPrefix(stdout.String(), wantSummary) || strings.Count(stdout.String(), "\n") != 1 {
				t.Fatalf("code %d summary %q stderr %q", code, stdout.String(), stderr.String())
			}
		})
	}
}
