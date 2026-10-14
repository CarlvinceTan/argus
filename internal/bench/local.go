package bench

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/carlvincetan/argus/internal/polymux"
)

type LocalRunner struct {
	Repo, Corpus string
	Timeout      time.Duration
}

const DefaultPassTimeout = 5 * time.Minute

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	data, err := cmd.Output()
	// Git stderr can contain authenticated URLs. Expose actionable categories,
	// never raw repository output or configured credentials.
	if err != nil {
		reason := "check repository access, pinned commit availability and patch compatibility"
		if exit, ok := err.(*exec.ExitError); ok {
			diagnostic := strings.ToLower(string(exit.Stderr))
			for _, hint := range []struct{ match, reason string }{
				{"not a git repository", "directory is not a Git repository"},
				{"does not appear to be a git repository", "source checkout is missing or is not a Git repository"},
				{"couldn't find remote ref", "pinned commit is unavailable in the source checkout"},
				{"not our ref", "pinned commit is unavailable in the source checkout"},
				{"invalid reference", "pinned commit is unavailable in the scratch repository"},
				{"patch does not apply", "planted patch does not match the pinned source"},
				{"corrupt patch", "planted patch is malformed"},
				{"permission denied", "Git cannot access the source or output directory"},
				{"could not read", "Git could not read the source; check local checkout access"},
			} {
				if strings.Contains(diagnostic, hint.match) {
					reason = hint.reason
					break
				}
			}
		}
		return "", fmt.Errorf("git %s failed: %s: %w", args[0], reason, err)
	}
	return string(data), nil
}

func (r *LocalRunner) Prepare(ctx context.Context, b Build, out string) (string, func() error, error) {
	repo, err := filepath.Abs(r.Repo)
	if err != nil {
		return "", nil, err
	}
	scratch := filepath.Join(out, "scratch.git")
	work := filepath.Join(out, "worktree")
	cleanup := func() error {
		// A canceled benchmark still removes its worktree registration.
		cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, removeErr := git(cleanCtx, scratch, "worktree", "remove", "--force", work)
		if removeErr != nil {
			return removeErr
		}
		return os.RemoveAll(scratch)
	}
	// Own the scratch Git database as well: no writes, locks or worktree metadata
	// are left in the caller's checkout, including on cvmini's read-only sources.
	if _, err = git(ctx, out, "init", "--bare", scratch); err != nil {
		return "", nil, err
	}
	if _, err = git(ctx, scratch, "fetch", "--no-tags", "--depth=1", repo, b.Commit); err != nil {
		_ = os.RemoveAll(scratch)
		return "", nil, err
	}
	if _, err = git(ctx, scratch, "worktree", "add", "--detach", work, b.Commit); err != nil {
		_ = os.RemoveAll(scratch)
		return "", nil, err
	}
	if b.Patch != "" {
		patch, e := filepath.Abs(filepath.Join(r.Corpus, b.Patch))
		if e != nil {
			err = e
		} else {
			_, err = git(ctx, work, "apply", "--", patch)
		}
		if err != nil {
			_ = cleanup()
			return "", nil, err
		}
	}
	return work, cleanup, nil
}

func findingFor(test string) Finding {
	class, flow := polymux.ClassifyTest(test)
	return Finding{Class: class, Flow: flow, Test: test}
}

// Pass uses S1's complete server lane, including its coverage artifacts.
func (r *LocalRunner) Pass(ctx context.Context, work, out string) (Pass, error) {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = DefaultPassTimeout
	}
	if timeout < 0 {
		return Pass{}, fmt.Errorf("pass timeout must be positive")
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := polymux.Run(runCtx, polymux.Options{Repo: work, Out: out}, nil)
	if err != nil {
		return Pass{}, err
	}
	if runCtx.Err() != nil {
		return Pass{}, fmt.Errorf("server pass: %w", runCtx.Err())
	}
	if !result.Complete {
		return Pass{}, fmt.Errorf("server eval did not complete (build/startup/runner failure)")
	}
	for test, outcome := range result.Outcomes {
		if outcome != "pass" && outcome != "fail" {
			return Pass{}, fmt.Errorf("server eval skipped: %s", test)
		}
	}
	pass := Pass{Findings: []Finding{}, Outcomes: result.Outcomes, WallSeconds: result.Duration,
		LaneSeconds: map[string]float64{"server": result.Duration}, Resources: result.Resources}
	for test, outcome := range result.Outcomes {
		if outcome == "fail" {
			pass.Findings = append(pass.Findings, findingFor(test))
		}
	}
	sortFindings(pass.Findings)
	return pass, writeJSON(filepath.Join(out, "pass.json"), pass)
}
