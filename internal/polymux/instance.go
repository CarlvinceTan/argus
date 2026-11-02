package polymux

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/carlvincetan/argus/internal/sensors"
)

const (
	instanceStart = 10 * time.Minute // the first start builds the app
	instanceStop  = 30 * time.Second
)

// Instance is one throwaway app from scripts/run.sh: its own data, port and
// preferences, deleted when it stops. Tests never touch a personal install.
type Instance struct {
	Dir     string
	command *exec.Cmd
	done    chan error
}

// Doctor refuses a run that could only produce noise, and reports the machine's
// load so that any timing can be read against it.
func Doctor() (load float64, cores int, err error) {
	cores = runtime.NumCPU()
	output, err := exec.Command("ioreg", "-n", "Root", "-d1", "-a").Output()
	if err == nil && bytes.Contains(output, []byte("CGSSessionScreenIsLocked")) {
		return 0, cores, errors.New("the screen is locked, and macOS hides accessibility trees until it is unlocked")
	}
	output, err = exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0, cores, nil
	}
	fields := strings.Fields(strings.Trim(strings.TrimSpace(string(output)), "{}"))
	if len(fields) > 0 {
		load, _ = strconv.ParseFloat(fields[0], 64)
	}
	return load, cores, nil
}

var logWarnings = regexp.MustCompile(`(?i)(publishing changes from|modifying state during|attributegraph: cycle|invalid frame dimension|fault:|fatal error|unable to simultaneously|hang detected|main thread checker)`)

// ScanLog reports what the app said about itself. The same warning is one finding,
// however many times it repeats.
func ScanLog(path string) []sensors.Evidence {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	found := []sensors.Evidence{}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		match := logWarnings.FindString(line)
		key := strings.ToLower(match)
		if match == "" || seen[key] {
			continue
		}
		seen[key] = true
		found = append(found, sensors.Evidence{
			Check: sensors.LogWarning, Detail: "the app logged: " + strings.TrimSpace(line),
			Snippet: key, Severity: 2, Confidence: 0.8,
		})
	}
	return found
}

// BuildApp compiles the app once, so that every journey tests the same build even
// if the source tree moves while the swarm runs.
func BuildApp(ctx context.Context, repo, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	app := filepath.Join(dir, "Polymux.app")
	if err := os.RemoveAll(app); err != nil {
		return "", err
	}
	output, err := exec.CommandContext(ctx, filepath.Join(repo, "scripts/check.sh"), "macos", app).CombinedOutput()
	logPath := filepath.Join(dir, "build.log")
	if werr := os.WriteFile(logPath, output, 0o644); werr != nil {
		return "", werr
	}
	if err != nil {
		return "", fmt.Errorf("the app does not build, so nothing was tested; see %s", logPath)
	}
	return app, nil
}

// StartInstance launches a fixture instance of a prebuilt app and waits until it
// is running. Stopping is by signal, not context: only a clean exit deletes its data.
func StartInstance(ctx context.Context, repo, app string, flags []string, logPath string) (*Instance, error) {
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	command := exec.Command(filepath.Join(repo, "scripts/run.sh"), append([]string{"--fixture"}, flags...)...)
	command.Dir = repo
	command.Env = append(os.Environ(), "POLYMUX_E2E_PREBUILT=1", "POLYMUX_E2E_APP="+app)
	command.Stderr = logFile
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("starting scripts/run.sh: %w", err)
	}
	instance := &Instance{command: command, done: make(chan error, 1)}
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		dir := ""
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Fprintln(logFile, line)
			if value, ok := strings.CutPrefix(line, "  data:"); ok {
				dir = strings.TrimSuffix(strings.TrimSpace(value), "/data")
			}
			if strings.HasPrefix(line, "Press Ctrl-C") {
				ready <- dir
			}
		}
		instance.done <- command.Wait()
		close(ready)
	}()
	select {
	case dir, ok := <-ready:
		if !ok || dir == "" {
			return nil, fmt.Errorf("scripts/run.sh exited before the app ran; see %s", logPath)
		}
		instance.Dir = dir
		return instance, nil
	case <-ctx.Done():
		_ = instance.Stop()
		return nil, ctx.Err()
	case <-time.After(instanceStart):
		_ = instance.Stop()
		return nil, fmt.Errorf("scripts/run.sh did not start within %s; see %s", instanceStart, logPath)
	}
}

// Stop ends the runner, which deletes the instance, and checks that it did.
func (i *Instance) Stop() error {
	if i.command.Process == nil {
		return nil
	}
	if err := i.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-i.done:
	case <-time.After(instanceStop):
		return fmt.Errorf("scripts/run.sh (pid %d) did not stop within %s", i.command.Process.Pid, instanceStop)
	}
	if i.Dir != "" {
		if _, err := os.Stat(i.Dir); err == nil {
			return fmt.Errorf("instance %s survived its runner", i.Dir)
		}
	}
	return nil
}
