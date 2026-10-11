package webui

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LaunchedRun is an audit started from the web UI, as the API reports it. It is a
// plain value so it can be copied safely; the mutable state lives in launchedRun.
type LaunchedRun struct {
	ID         string     `json:"id"`
	Subject    string     `json:"subject"`
	Command    string     `json:"command"`
	Mode       string     `json:"mode"`
	Provider   string     `json:"provider"`
	Steps      int        `json:"steps"`
	Dir        string     `json:"dir"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	ExitCode   *int       `json:"exitCode,omitempty"`
	Status     string     `json:"status"`
	Log        []string   `json:"log"`
	Error      string     `json:"error,omitempty"`
	Running    bool       `json:"running"`
}

// launchedRun is the live state behind a LaunchedRun.
type launchedRun struct {
	mu      sync.Mutex
	run     LaunchedRun
	command *exec.Cmd
}

func (l *launchedRun) snapshot() LaunchedRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.run
	out.Log = append([]string{}, l.run.Log...)
	out.Running = l.command != nil && out.FinishedAt == nil
	return out
}

// LaunchRequest describes an audit to start.
type LaunchRequest struct {
	Subject     string            `json:"subject"`
	Command     string            `json:"command"`
	Mode        string            `json:"mode"`
	Provider    string            `json:"provider"`
	Steps       int               `json:"steps"`
	Cols        int               `json:"cols"`
	Rows        int               `json:"rows"`
	Display     string            `json:"display"`
	AgentAddr   string            `json:"agentAddr"`
	Window      string            `json:"window"`
	Offline     bool              `json:"offline"`
	Vision      bool              `json:"vision"`
	TextInput   bool              `json:"textInput"`
	AllowClicks bool              `json:"allowClicks"`
	Personas    []string          `json:"personas"`
	Env         map[string]string `json:"env"`
	OutputDir   string            `json:"outputDir"`
}

const maxLaunchedLogLines = 800

// SetArgusBinary points the launcher at the argus executable.
func (s *Service) SetArgusBinary(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.argusBinary = path
}

// Launch starts an audit run as a subprocess and streams its output into the log.
func (s *Service) Launch(request LaunchRequest) (LaunchedRun, error) {
	if strings.TrimSpace(request.Subject) == "" || strings.TrimSpace(request.Command) == "" {
		return LaunchedRun{}, fmt.Errorf("subject and command are required")
	}
	s.mu.Lock()
	if s.runs == nil {
		s.runs = map[string]*launchedRun{}
	}
	s.seq++
	id := fmt.Sprintf("launch-%d", s.seq)
	binary := s.argusBinary
	s.mu.Unlock()
	if binary == "" {
		binary = findArgusBinary()
	}
	if binary == "" {
		return LaunchedRun{}, fmt.Errorf("cannot find the argus executable; start argus-web with --argus-bin")
	}

	mode := request.Mode
	if mode == "" {
		mode = "tui"
	}
	provider := request.Provider
	if provider == "" {
		provider = "commandcode"
	}
	outDir := request.OutputDir
	if outDir == "" {
		outDir = filepath.Join(s.config.RunDir, slugify(request.Subject)+"-"+time.Now().Format("20060102-150405"))
	}
	steps := request.Steps
	if steps <= 0 {
		steps = 6
	}
	columns, rows := request.Cols, request.Rows
	if columns <= 0 {
		columns = 100
	}
	if rows <= 0 {
		rows = 30
	}

	args := []string{
		"run",
		"--subject", request.Subject,
		"--cmd", request.Command,
		"--mode", mode,
		"--provider", provider,
		"--steps", fmt.Sprintf("%d", steps),
		"--out", outDir,
	}
	if mode == "gui" {
		if request.Display != "" {
			args = append(args, "--display", request.Display)
		}
		if request.AgentAddr != "" {
			args = append(args, "--x11-agent", request.AgentAddr)
		}
		if request.Window != "" {
			args = append(args, "--window", request.Window)
		}
		if !request.AllowClicks {
			args = append(args, "--clicks=false")
		}
	} else {
		args = append(args, "--cols", fmt.Sprintf("%d", columns), "--rows", fmt.Sprintf("%d", rows))
		if request.TextInput {
			args = append(args, "--text-input")
		}
	}
	if request.Offline {
		args = append(args, "--offline")
	}
	if !request.Vision {
		args = append(args, "--vision=false")
	}
	for _, persona := range request.Personas {
		args = append(args, "--persona", persona)
	}
	for name, value := range request.Env {
		args = append(args, "--env", name+"="+value)
	}

	launched := &launchedRun{run: LaunchedRun{
		ID:        id,
		Subject:   request.Subject,
		Command:   strings.Join(append([]string{binary}, args...), " "),
		Mode:      mode,
		Provider:  provider,
		Steps:     steps,
		Dir:       outDir,
		StartedAt: time.Now(),
		Status:    "running",
		Log:       []string{fmt.Sprintf("$ %s %s", binary, strings.Join(args, " "))},
	}}
	command := exec.Command(binary, args...)
	command.Dir = workingDirectory()
	command.Env = os.Environ()
	stdout, err := command.StdoutPipe()
	if err != nil {
		return LaunchedRun{}, err
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		launched.mu.Lock()
		launched.run.Status = "failed"
		launched.run.Error = err.Error()
		launched.mu.Unlock()
		return launched.snapshot(), nil
	}
	launched.command = command

	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			launched.appendLog(scanner.Text())
		}
		err := command.Wait()
		launched.mu.Lock()
		defer launched.mu.Unlock()
		finished := time.Now()
		launched.run.FinishedAt = &finished
		code := command.ProcessState.ExitCode()
		launched.run.ExitCode = &code
		switch {
		case err != nil && code != 0:
			launched.run.Status = "failed"
			launched.run.Error = err.Error()
		case launched.run.Status == "stopped":
			// left as stopped
		default:
			launched.run.Status = "done"
		}
	}()

	s.mu.Lock()
	s.runs[id] = launched
	s.mu.Unlock()
	return launched.snapshot(), nil
}

// LaunchedRuns lists the audits started from the UI, newest first.
func (s *Service) LaunchedRuns() []LaunchedRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]LaunchedRun, 0, len(s.runs))
	for _, launched := range s.runs {
		out = append(out, launched.snapshot())
	}
	sortLaunched(out)
	return out
}

// StopLaunched terminates a running audit.
func (s *Service) StopLaunched(id string) error {
	s.mu.Lock()
	launched, ok := s.runs[id]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no run %q", id)
	}
	launched.mu.Lock()
	defer launched.mu.Unlock()
	if launched.command == nil || launched.command.Process == nil {
		return fmt.Errorf("run %q is not running", id)
	}
	if err := launched.command.Process.Kill(); err != nil {
		return err
	}
	launched.run.Status = "stopped"
	return nil
}

func (l *launchedRun) appendLog(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.run.Log = append(l.run.Log, line)
	if len(l.run.Log) > maxLaunchedLogLines {
		l.run.Log = l.run.Log[len(l.run.Log)-maxLaunchedLogLines:]
	}
}

func sortLaunched(runs []LaunchedRun) {
	for i := 1; i < len(runs); i++ {
		for j := i; j > 0 && runs[j].StartedAt.After(runs[j-1].StartedAt); j-- {
			runs[j], runs[j-1] = runs[j-1], runs[j]
		}
	}
}

// findArgusBinary looks for the tool next to the checkout the web UI serves.
func findArgusBinary() string {
	candidates := []string{
		filepath.Join(workingDirectory(), "var", "argus"),
		filepath.Join(workingDirectory(), "argus"),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			absolute, err := filepath.Abs(candidate)
			if err == nil {
				return absolute
			}
			return candidate
		}
	}
	if path, err := exec.LookPath("argus"); err == nil {
		return path
	}
	return ""
}

func workingDirectory() string {
	if directory, err := os.Getwd(); err == nil {
		return directory
	}
	return "."
}

func slugify(text string) string {
	builder := strings.Builder{}
	for _, char := range strings.ToLower(strings.TrimSpace(text)) {
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
