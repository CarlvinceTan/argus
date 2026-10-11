// Package harness drives a TUI subject on a real pseudo-terminal.
//
// It records three parallel traces: the raw output byte count, a frame ledger
// (one entry per screen change with a digest, a changed-cell count and the id of
// the last input that preceded it) and a ring buffer of full grids. The frame
// ledger is what animation analysis reads; the grids are what rendering and
// deterministic checks read.
package harness

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"

	"github.com/carlvincetan/argus/internal/screen"
)

// FrameEvent is one observed terminal screen change (see screen.FrameEvent).
type FrameEvent = screen.FrameEvent

// InputEvent is one keystroke, text or resize sent to the subject
// (see screen.InputEvent).
type InputEvent = screen.InputEvent

// Options configures a harness run.
type Options struct {
	Argv           []string
	Cols           int
	Rows           int
	Dir            string
	Env            []string
	MaxGrids       int
	SandboxProfile string
	ExtraFiles     []*os.File
}

// Result summarises a finished run.
type Result struct {
	ExitCode     int           `json:"exit_code"`
	Exited       bool          `json:"exited"`
	OutputBytes  int           `json:"output_bytes"`
	Duration     time.Duration `json:"duration_ms"`
	FrameCount   int           `json:"frame_count"`
	InputCount   int           `json:"input_count"`
	FramesPerSec float64       `json:"frames_per_sec"`
}

// Harness owns one subject process on a pty.
type Harness struct {
	opts Options

	cmd  *exec.Cmd
	pty  *os.File
	term vt10x.Terminal

	mu        sync.Mutex
	grids     []*screen.Grid
	frames    []FrameEvent
	inputs    []InputEvent
	lastGrid  *screen.Grid
	lastDgest string
	inputSeq  int
	bytes     int
	start     time.Time
	exited    bool
	exitCode  int
	waitErr   error

	done chan struct{}
}

// Start launches the subject.
func Start(opts Options) (*Harness, error) {
	if len(opts.Argv) == 0 {
		return nil, errors.New("harness: empty argv")
	}
	if opts.Cols <= 0 {
		opts.Cols = 100
	}
	if opts.Rows <= 0 {
		opts.Rows = 30
	}
	if opts.MaxGrids <= 0 {
		opts.MaxGrids = 800
	}

	argv := append([]string{}, opts.Argv...)
	if opts.SandboxProfile != "" {
		argv = append([]string{"/usr/bin/sandbox-exec", "-f", opts.SandboxProfile, "--"}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	if cmd.Env == nil {
		cmd.Env = SubjectEnv()
	}

	ptyFile, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(opts.Cols), Rows: uint16(opts.Rows)})
	if err != nil {
		return nil, fmt.Errorf("starting subject on pty: %w", err)
	}

	h := &Harness{
		opts:  opts,
		cmd:   cmd,
		pty:   ptyFile,
		term:  vt10x.New(vt10x.WithSize(opts.Cols, opts.Rows)),
		start: time.Now(),
		done:  make(chan struct{}),
	}
	go h.readLoop()
	go h.waitLoop()
	return h, nil
}

// SubjectEnv builds a clean environment for the subject. Argus's own
// credentials and configuration are stripped so a subject can never read them
// (and so a benchmark subject cannot look up its own answer).
func SubjectEnv() []string {
	blocked := []string{
		"OPENROUTER", "OPENAI", "ANTHROPIC", "GEMINI", "GOOGLE_API", "TYPESAFE",
		"VERCEL", "ARGUS", "DEEPSEEK", "MISTRAL", "GROQ", "XAI", "TOGETHER",
	}
	env := []string{
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
		"TERMINFO_DIRS=/usr/share/terminfo",
	}
	home, err := os.UserHomeDir()
	if err == nil {
		env = append(env, "HOME="+home)
	}
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}
	env = append(env, "PATH="+path, "SHELL=/bin/sh")
	for _, entry := range os.Environ() {
		name, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		skip := false
		for _, prefix := range blocked {
			if strings.HasPrefix(strings.ToUpper(name), prefix) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		switch name {
		case "TMPDIR", "USER", "LOGNAME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME":
			env = append(env, entry)
		}
	}
	return env
}

func (h *Harness) readLoop() {
	reader := bufio.NewReaderSize(h.pty, 64*1024)
	buffer := make([]byte, 64*1024)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			h.mu.Lock()
			h.bytes += n
			_, _ = h.term.Write(buffer[:n])
			h.observeLocked()
			h.mu.Unlock()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !isClosedErr(err) {
				// The pty closing is the normal end of a run.
			}
			return
		}
	}
}

func (h *Harness) waitLoop() {
	err := h.cmd.Wait()
	h.mu.Lock()
	h.exited = true
	h.waitErr = err
	if h.cmd.ProcessState != nil {
		h.exitCode = h.cmd.ProcessState.ExitCode()
	}
	h.mu.Unlock()
	close(h.done)
}

func (h *Harness) observeLocked() {
	grid := screen.Capture(h.term, time.Since(h.start), len(h.grids))
	if h.lastGrid != nil && grid.Digest == h.lastDgest {
		return
	}
	changed := 0
	if h.lastGrid != nil {
		changed = grid.Changed(h.lastGrid)
	}
	h.frames = append(h.frames, FrameEvent{
		T:            grid.Time,
		Digest:       grid.Digest,
		ChangedCells: changed,
		GridID:       grid.ID,
		InputSeq:     h.inputSeq,
	})
	h.grids = append(h.grids, grid)
	if len(h.grids) > h.opts.MaxGrids {
		// Keep the most recent grids; frame events retain the full history.
		h.grids = h.grids[len(h.grids)-h.opts.MaxGrids:]
	}
	h.lastGrid = grid
	h.lastDgest = grid.Digest
}

// SendRaw writes bytes to the subject.
func (h *Harness) SendRaw(data []byte) error {
	return h.send(data, "raw")
}

// SendText types literal text.
func (h *Harness) SendText(text string) error {
	return h.send([]byte(text), "text")
}

// SendKey sends a named key (ENTER, TAB, ESC, UP, DOWN, LEFT, RIGHT, CTRL+C, ...).
func (h *Harness) SendKey(key string) error {
	encoded, err := EncodeKey(key)
	if err != nil {
		return err
	}
	return h.send(encoded, "key")
}

func (h *Harness) send(data []byte, kind string) error {
	h.mu.Lock()
	h.inputSeq++
	seq := h.inputSeq
	h.inputs = append(h.inputs, InputEvent{
		T:    time.Since(h.start),
		Seq:  seq,
		Kind: kind,
		Data: printable(data),
	})
	h.mu.Unlock()
	if _, err := h.pty.Write(data); err != nil {
		return fmt.Errorf("writing to subject: %w", err)
	}
	return nil
}

// Resize changes the terminal size.
func (h *Harness) Resize(cols, rows int) error {
	if err := pty.Setsize(h.pty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		return fmt.Errorf("resizing subject: %w", err)
	}
	h.term.Resize(cols, rows)
	h.mu.Lock()
	h.inputSeq++
	h.inputs = append(h.inputs, InputEvent{
		T:    time.Since(h.start),
		Seq:  h.inputSeq,
		Kind: "resize",
		Data: fmt.Sprintf("%dx%d", cols, rows),
	})
	h.mu.Unlock()
	return nil
}

// Snapshot captures the current screen and retains it, so a finding made on a
// snapshot can still be rendered as evidence later.
func (h *Harness) Snapshot() *screen.Grid {
	h.mu.Lock()
	defer h.mu.Unlock()
	grid := screen.Capture(h.term, time.Since(h.start), len(h.grids))
	h.grids = append(h.grids, grid)
	if len(h.grids) > h.opts.MaxGrids {
		h.grids = h.grids[len(h.grids)-h.opts.MaxGrids:]
	}
	return grid
}

// Grids returns the retained grid history.
func (h *Harness) Grids() []*screen.Grid {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*screen.Grid, len(h.grids))
	copy(out, h.grids)
	return out
}

// Frames returns the frame ledger.
func (h *Harness) Frames() []FrameEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]FrameEvent, len(h.frames))
	copy(out, h.frames)
	return out
}

// Inputs returns the input trace.
func (h *Harness) Inputs() []InputEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]InputEvent, len(h.inputs))
	copy(out, h.inputs)
	return out
}

// WaitForChange waits until the screen digest differs from the current one.
func (h *Harness) WaitForChange(timeout time.Duration) bool {
	h.mu.Lock()
	before := h.lastDgest
	h.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		h.mu.Lock()
		changed := h.lastDgest != before
		h.mu.Unlock()
		if changed {
			return true
		}
	}
	return false
}

// WaitIdle waits until the screen has not changed for the quiet period.
func (h *Harness) WaitIdle(quiet, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	lastChange := time.Now()
	sawDigest := ""
	for time.Now().Before(deadline) {
		h.mu.Lock()
		digest := h.lastDgest
		h.mu.Unlock()
		if digest != sawDigest {
			sawDigest = digest
			lastChange = time.Now()
		} else if time.Since(lastChange) >= quiet {
			return true
		}
		time.Sleep(15 * time.Millisecond)
	}
	return false
}

// SampleSequence captures grids spaced by the given interval.
func (h *Harness) SampleSequence(count int, interval time.Duration) []*screen.Grid {
	out := make([]*screen.Grid, 0, count)
	for index := 0; index < count; index++ {
		out = append(out, h.Snapshot())
		if index < count-1 {
			time.Sleep(interval)
		}
	}
	return out
}

// Now returns the elapsed time since the harness started.
func (h *Harness) Now() time.Duration {
	return time.Since(h.start)
}

// Exited reports whether the subject has exited, with its code.
func (h *Harness) Exited() (bool, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exited, h.exitCode
}

// Wait blocks until the subject exits or the timeout elapses.
func (h *Harness) Wait(timeout time.Duration) bool {
	select {
	case <-h.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Result summarises the run so far.
func (h *Harness) Result() Result {
	h.mu.Lock()
	defer h.mu.Unlock()
	duration := time.Since(h.start)
	result := Result{
		ExitCode:    h.exitCode,
		Exited:      h.exited,
		OutputBytes: h.bytes,
		Duration:    duration,
		FrameCount:  len(h.frames),
		InputCount:  len(h.inputs),
	}
	if duration > 0 {
		result.FramesPerSec = float64(len(h.frames)) / duration.Seconds()
	}
	return result
}

// Close terminates the subject and releases the pty.
func (h *Harness) Close() error {
	if h.pty != nil {
		_ = h.pty.Close()
	}
	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

// EncodeKey maps a key name to the bytes a terminal would send.
func EncodeKey(key string) ([]byte, error) {
	specials := map[string]string{
		"ENTER":     "\r",
		"RETURN":    "\r",
		"TAB":       "\t",
		"ESC":       "\x1b",
		"ESCAPE":    "\x1b",
		"SPACE":     " ",
		"BACKSPACE": "\x7f",
		"UP":        "\x1b[A",
		"DOWN":      "\x1b[B",
		"RIGHT":     "\x1b[C",
		"LEFT":      "\x1b[D",
		"HOME":      "\x1b[H",
		"END":       "\x1b[F",
		"PGUP":      "\x1b[5~",
		"PGDN":      "\x1b[6~",
		"PAGEUP":    "\x1b[5~",
		"PAGEDOWN":  "\x1b[6~",
		"DEL":       "\x1b[3~",
		"DELETE":    "\x1b[3~",
		"F1":        "\x1bOP",
		"F2":        "\x1bOQ",
		"F3":        "\x1bOR",
		"F4":        "\x1bOS",
		"F5":        "\x1b[15~",
		"F6":        "\x1b[17~",
		"INSERT":    "\x1b[2~",
		"SHIFT+TAB": "\x1b[Z",
	}
	upper := strings.ToUpper(strings.TrimSpace(key))
	if value, ok := specials[upper]; ok {
		return []byte(value), nil
	}
	if strings.HasPrefix(upper, "CTRL+") || strings.HasPrefix(upper, "C-") {
		letter := strings.TrimPrefix(strings.TrimPrefix(upper, "CTRL+"), "C-")
		if len(letter) == 1 && letter[0] >= 'A' && letter[0] <= 'Z' {
			return []byte{letter[0] - 64}, nil
		}
		if letter == "@" || letter == "SPACE" {
			return []byte{0}, nil
		}
		return nil, fmt.Errorf("harness: unsupported control key %q", key)
	}
	if rest, ok := stripPrefixFold(key, "ALT+"); ok {
		return altSequence(rest, key)
	}
	if rest, ok := stripPrefixFold(key, "M-"); ok {
		return altSequence(rest, key)
	}
	if len(key) == 1 {
		return []byte(key), nil
	}
	return nil, fmt.Errorf("harness: unknown key %q", key)
}

// altSequence encodes Alt+<letter> with the letter's original case.
func altSequence(rest, original string) ([]byte, error) {
	rest = strings.TrimSpace(rest)
	if len(rest) == 1 {
		return []byte{0x1b, rest[0]}, nil
	}
	return nil, fmt.Errorf("harness: unsupported alt key %q", original)
}

func stripPrefixFold(value, prefix string) (string, bool) {
	if len(value) < len(prefix) {
		return "", false
	}
	if !strings.EqualFold(value[:len(prefix)], prefix) {
		return "", false
	}
	return value[len(prefix):], true
}

func printable(data []byte) string {
	var builder strings.Builder
	for _, b := range data {
		if b >= 32 && b < 127 {
			builder.WriteByte(b)
			continue
		}
		fmt.Fprintf(&builder, "\\x%02x", b)
	}
	return builder.String()
}

func isClosedErr(err error) bool {
	return errors.Is(err, os.ErrClosed) || strings.Contains(err.Error(), "file already closed")
}
