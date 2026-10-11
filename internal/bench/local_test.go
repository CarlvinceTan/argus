package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScratchWorktreeAppliesPatchAndPreservesCheckout(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "source")
	corpus := filepath.Join(root, "corpus")
	out := filepath.Join(root, "out")
	for _, dir := range []string{repo, corpus, out} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := git(ctx, repo, "init"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(repo, "subject.go")
	if err := os.WriteFile(file, []byte("package subject\nconst Reply = true\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repo, "add", "subject.go"); err != nil {
		t.Fatal(err)
	}
	// Only this disposable test repository gets a fixture commit.
	if _, err := git(ctx, repo, "-c", "user.name=Bench Test", "-c", "user.email=bench@example.invalid", "commit", "-m", "fixture"); err != nil {
		t.Fatal(err)
	}
	sha, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	patch := "--- a/subject.go\n+++ b/subject.go\n@@ -1,2 +1,2 @@\n package subject\n-const Reply = true\n+const Reply = false\n"
	if err := os.WriteFile(filepath.Join(corpus, "bug.patch"), []byte(patch), 0644); err != nil {
		t.Fatal(err)
	}
	// Dirty caller changes must not leak into the pinned benchmark build.
	if err := os.WriteFile(file, []byte("dirty caller file\n"), 0644); err != nil {
		t.Fatal(err)
	}
	r := LocalRunner{Repo: repo, Corpus: corpus}
	work, cleanup, err := r.Prepare(ctx, Build{Commit: strings.TrimSpace(sha), Patch: "bug.patch"}, out)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(work, "subject.go"))
	if err != nil || !strings.Contains(string(data), "Reply = false") {
		t.Fatalf("patch not applied: %s %v", data, err)
	}
	data, err = os.ReadFile(file)
	if err != nil || string(data) != "dirty caller file\n" {
		t.Fatal("caller checkout modified")
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatal("worktree remains")
	}
	list, err := git(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil || strings.Count(list, "worktree ") != 1 {
		t.Fatal("external worktree registration remains")
	}
}

// Exercise the complete local pipeline with a disposable checkout and a fake
// eval process. No external checkout, model, network listener or opt-in is needed.
func TestLocalBenchmarkScoresPatchedScratchWorktree(t *testing.T) {
	opts, m := corpus(t)
	repo := filepath.Join(t.TempDir(), "source")
	evals := filepath.Join(repo, "apps", "server", "evals")
	if err := os.MkdirAll(evals, 0755); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := git(ctx, repo, "init"); err != nil {
		t.Fatal(err)
	}
	source := "package evals\nimport \"testing\"\nfunc TestT2EndTurn(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(evals, "scenarios_test.go"), []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	subject := filepath.Join(repo, "apps", "server", "subject.go")
	if err := os.WriteFile(subject, []byte("package subject\nconst Reply = true\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repo, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repo, "-c", "user.name=Bench Test", "-c", "user.email=bench@example.invalid", "commit", "-m", "fixture"); err != nil {
		t.Fatal(err)
	}
	sha, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	for i := range m.Builds {
		m.Builds[i].Commit = strings.TrimSpace(sha)
	}
	if err := writeJSON(filepath.Join(opts.Corpus, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}
	patch := "--- a/apps/server/subject.go\n+++ b/apps/server/subject.go\n@@ -1,2 +1,2 @@\n package subject\n-const Reply = true\n+const Reply = false\n"
	if err := os.WriteFile(filepath.Join(opts.Corpus, "bug.patch"), []byte(patch), 0644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := `#!/bin/sh
[ "$*" = 'test -tags sqlite_fts5 -count=1 -json ./evals/' ] || exit 9
case "$(cat subject.go)" in
  *'Reply = false'*) result=fail; status=1 ;;
  *'Reply = true'*) result=pass; status=0 ;;
  *) exit 8 ;;
esac
printf '{"Action":"%s","Test":"TestT2EndTurn"}\n' "$result"
printf '{"Action":"%s","Package":"evals"}\n' "$result"
exit "$status"
`
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	opts.Polymux = repo
	got, err := Run(ctx, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Caught != 1 || got.Planted != 1 || got.FalsePositives != 0 || len(got.Flaky) != 0 || len(got.Samples) != 6 {
		t.Fatalf("local benchmark score: %+v", got)
	}
	for round := 1; round <= 3; round++ {
		for _, build := range m.Builds {
			out := filepath.Join(opts.Out, fmt.Sprintf("run-%d", round), build.ID)
			for _, scratch := range []string{"scratch.git", "worktree"} {
				if _, err := os.Stat(filepath.Join(out, scratch)); !os.IsNotExist(err) {
					t.Fatalf("scratch remains: %s/%s", out, scratch)
				}
			}
			for _, evidence := range []string{"pass.json", "coverage.json"} {
				if _, err := os.Stat(filepath.Join(out, evidence)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	data, err := os.ReadFile(subject)
	if err != nil || !strings.Contains(string(data), "Reply = true") {
		t.Fatal("benchmark changed source checkout")
	}
	previous, err := lastHistory(filepath.Join(filepath.Dir(opts.Corpus), "history.tsv"), got.Host)
	if err != nil || previous == nil || previous.Caught != 1 || previous.FalsePositives != 0 {
		t.Fatalf("local benchmark history: %+v %v", previous, err)
	}
}

func TestLocalPassUsesFullSuiteFlagsAndWritesEvidence(t *testing.T) {
	root := t.TempDir()
	evals := filepath.Join(root, "apps", "server", "evals")
	bin := filepath.Join(root, "bin")
	out := filepath.Join(root, "out")
	for _, dir := range []string{evals, bin, out} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	source := "package evals\nimport \"testing\"\nfunc TestT2EndTurn(t *testing.T) {}\nfunc TestB2BackgroundNotify(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(evals, "scenarios_test.go"), []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
[ "$*" = 'test -tags sqlite_fts5 -count=1 -json ./evals/' ] || exit 9
cat <<'JSON'
{"Action":"fail","Test":"TestT2EndTurn/sub"}
{"Action":"fail","Test":"TestT2EndTurn"}
{"Action":"pass","Test":"TestB2BackgroundNotify"}
{"Action":"fail","Package":"evals"}
JSON
exit 1
`
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	p, err := (&LocalRunner{}).Pass(t.Context(), root, out)
	if err != nil || len(p.Findings) != 1 || p.WallSeconds <= 0 || p.LaneSeconds["server"] != p.WallSeconds {
		t.Fatalf("full suite pass: %+v %v", p, err)
	}
	for _, file := range []string{"pass.json", "coverage.json"} {
		data, err := os.ReadFile(filepath.Join(out, file))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "Action") {
			t.Fatal("raw server output retained")
		}
	}
}

func TestLocalPassRejectsIncompleteAndSkippedRuns(t *testing.T) {
	for _, kind := range []string{"build failure", "startup", "truncated", "missing eval", "skipped eval", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			evals := filepath.Join(root, "apps", "server", "evals")
			bin, out := filepath.Join(root, "bin"), filepath.Join(root, "out")
			for _, dir := range []string{evals, bin, out} {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(evals, "scenarios_test.go"), []byte("package evals\nimport \"testing\"\nfunc TestT2EndTurn(t *testing.T) {}\n"), 0644); err != nil {
				t.Fatal(err)
			}
			script := "#!/bin/sh\n"
			switch kind {
			case "build failure":
				script += "printf '{\"Action\":\"fail\",\"Package\":\"evals\"}\\n'\nexit 1\n"
			case "startup":
				script += "exit 1\n"
			case "truncated":
				script += "printf '{\"Action\":\"pass\",\"Test\":\"TestT2EndTurn\"}\\n'\n"
			case "missing eval":
				script += "printf '{\"Action\":\"pass\",\"Package\":\"evals\"}\\n'\n"
			case "skipped eval":
				script += "printf '{\"Action\":\"skip\",\"Test\":\"TestT2EndTurn\"}\\n{\"Action\":\"pass\",\"Package\":\"evals\"}\\n'\n"
			case "timeout":
				script += "exec /bin/sleep 5\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			runner := &LocalRunner{Timeout: time.Second}
			if kind == "timeout" {
				runner.Timeout = 20 * time.Millisecond
			}
			if _, err := runner.Pass(t.Context(), root, out); err == nil {
				t.Fatal("incomplete or skipped eval was scored")
			}
			if _, err := os.Stat(filepath.Join(out, "pass.json")); !os.IsNotExist(err) {
				t.Fatal("invalid pass artifact written")
			}
			if _, err := os.Stat(filepath.Join(out, "coverage.json")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
