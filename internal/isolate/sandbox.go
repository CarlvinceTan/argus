// Package isolate builds the sandbox profiles Argus runs under and proves they
// do what they claim.
//
// Two profiles matter:
//
//   - SubjectProfile: the program under test runs with all network access
//     denied. A UI test never needs the network, and a benchmark subject must
//     not be able to look up its own bug report.
//   - EvalProfile: for offline evaluation the whole harness runs with network
//     denied except loopback, so inference must come from the local model.
//
// Profiles are only text; the Canary function is the evidence. It runs real
// network probes from inside the sandbox and records which ones failed.
package isolate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SubjectProfileText denies all network access while leaving the filesystem and
// process model intact, so ordinary terminal programs still run.
const SubjectProfileText = `(version 1)
;; Everything a normal program needs stays allowed; the network does not.
;; Whitelisting individual operations was tried first and broke the subject
;; (a Go program panicked on a denied syscall), so the profile is expressed as
;; "allow by default, then deny the network".
(allow default)
(deny network*)
;; Loopback stays available so a subject can talk to a local model or helper.
;; Nothing here can reach the internet, which is what the fairness claim needs.
(allow network-inbound (local ip "localhost:*"))
(allow network-outbound (remote ip "localhost:*"))
`

// EvalProfileTemplate denies everything except loopback traffic to the local
// model server. It is applied to the Argus process itself during an offline
// evaluation, which is what makes the "no internet during testing" claim
// checkable.
const EvalProfileTemplate = `(version 1)
;; Offline evaluation: no network except the loopback model server.
(allow default)
(deny network*)
(allow network-inbound (local ip "localhost:*"))
(allow network-outbound (remote ip "localhost:%d"))
`

// Profile is a written sandbox profile plus its hash.
type Profile struct {
	Path string
	Hash string
	Kind string
}

// WriteProfile writes a sandbox profile and returns its path and hash.
func WriteProfile(dir, name, content string) (Profile, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Profile{}, fmt.Errorf("creating profile directory: %w", err)
	}
	path := filepath.Join(dir, name+".sb")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return Profile{}, fmt.Errorf("writing sandbox profile: %w", err)
	}
	sum := sha256.Sum256([]byte(content))
	return Profile{Path: path, Hash: hex.EncodeToString(sum[:8]), Kind: name}, nil
}

// SubjectProfile writes the subject network-deny profile.
func SubjectProfile(dir string) (Profile, error) {
	return WriteProfile(dir, "subject-offline", SubjectProfileText)
}

// EvalProfile writes the loopback-only evaluation profile.
func EvalProfile(dir string, modelPort int) (Profile, error) {
	return WriteProfile(dir, "eval-loopback", fmt.Sprintf(EvalProfileTemplate, modelPort))
}

// CanaryResult is one probe.
type CanaryResult struct {
	Name     string `json:"name"`
	Expected string `json:"expected"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
	Passed   bool   `json:"passed"`
}

// CanaryEvidence is the record stored in a session.
type CanaryEvidence struct {
	Profile string         `json:"profile"`
	Hash    string         `json:"profile_hash"`
	Passed  bool           `json:"passed"`
	Results []CanaryResult `json:"results"`
	RanAt   time.Time      `json:"ran_at"`
}

// Probe runs the same network probes as Canary but without wrapping them in
// sandbox-exec. It is used from a process that is *already* inside the eval
// sandbox, where a nested sandbox-exec is not permitted but the answer is even
// more direct: this very process either can reach the network or it cannot.
func Probe(ctx context.Context, modelURL string) CanaryEvidence {
	evidence := CanaryEvidence{RanAt: time.Now()}
	probes := []struct {
		name   string
		url    string
		expect string
	}{
		{name: "internet_by_hostname", url: "https://openrouter.ai/api/v1/models", expect: "fail"},
		{name: "internet_by_ip", url: "http://1.1.1.1", expect: "fail"},
		{name: "dns_resolution", url: "http://example.com", expect: "fail"},
	}
	if modelURL != "" {
		probes = append(probes, struct {
			name   string
			url    string
			expect string
		}{name: "loopback_model", url: strings.TrimRight(modelURL, "/") + "/models", expect: "success"})
	}
	for _, item := range probes {
		command := exec.CommandContext(ctx, "/usr/bin/curl", "-sS", "-m", "4", "-o", "/dev/null", "-w", "%{http_code}", item.url)
		var output strings.Builder
		command.Stdout = &output
		command.Stderr = &output
		err := command.Run()
		exitCode := 0
		if err != nil {
			if exitError, ok := err.(*exec.ExitError); ok {
				exitCode = exitError.ExitCode()
			} else {
				exitCode = -1
			}
		}
		passed := (item.expect == "success" && exitCode == 0) || (item.expect == "fail" && exitCode != 0)
		evidence.Results = append(evidence.Results, CanaryResult{
			Name:     item.name,
			Expected: item.expect,
			ExitCode: exitCode,
			Output:   truncate(output.String(), 200),
			Passed:   passed,
		})
	}
	evidence.Passed = true
	for _, result := range evidence.Results {
		if !result.Passed {
			evidence.Passed = false
		}
	}
	return evidence
}

// Canary runs probes from inside the sandbox: DNS and internet must fail, and
// (when a model URL is given) the local model must still answer.
func Canary(ctx context.Context, profile Profile, modelURL string) CanaryEvidence {
	evidence := CanaryEvidence{Profile: profile.Path, Hash: profile.Hash, RanAt: time.Now()}

	type probe struct {
		name     string
		url      string
		expect   string
		curlArgs []string
	}
	probes := []probe{
		{name: "internet_by_hostname", url: "https://openrouter.ai/api/v1/models", expect: "fail"},
		{name: "internet_by_ip", url: "http://1.1.1.1", expect: "fail"},
		{name: "dns_resolution", url: "http://example.com", expect: "fail"},
	}
	if modelURL != "" {
		probes = append(probes, probe{name: "loopback_model", url: strings.TrimRight(modelURL, "/") + "/models", expect: "success"})
	}

	for _, item := range probes {
		args := []string{"-f", profile.Path, "--", "/usr/bin/curl", "-sS", "-m", "4", "-o", "/dev/null", "-w", "%{http_code}", item.url}
		command := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", args...)
		var output strings.Builder
		command.Stdout = &output
		command.Stderr = &output
		err := command.Run()
		exitCode := 0
		if err != nil {
			if exitError, ok := err.(*exec.ExitError); ok {
				exitCode = exitError.ExitCode()
			} else {
				exitCode = -1
			}
		}
		passed := (item.expect == "success" && exitCode == 0) || (item.expect == "fail" && exitCode != 0)
		evidence.Results = append(evidence.Results, CanaryResult{
			Name:     item.name,
			Expected: item.expect,
			ExitCode: exitCode,
			Output:   truncate(output.String(), 200),
			Passed:   passed,
		})
	}
	evidence.Passed = true
	for _, result := range evidence.Results {
		if !result.Passed {
			evidence.Passed = false
		}
	}
	return evidence
}

// SandboxAvailable reports whether sandbox-exec exists on this machine.
func SandboxAvailable() bool {
	_, err := os.Stat("/usr/bin/sandbox-exec")
	return err == nil
}

func truncate(text string, limit int) string {
	cleaned := strings.TrimSpace(text)
	if len(cleaned) <= limit {
		return cleaned
	}
	return cleaned[:limit] + "..."
}
