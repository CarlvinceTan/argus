// Control harness: drives one owned Polymux instance on macOS.
//
// Polymux's Control helper keeps real accessibility handles in a stdio session.
// There is no shared daemon and it never takes focus, so several instances can
// run side by side. It refuses anything but an instance from scripts/run.sh,
// which is what keeps a test run away from a personal app.
package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/carlvincetan/argus/internal/screen"
)

const (
	controlMaxFrames = 24
	controlPoll      = 100 * time.Millisecond
	controlRetries   = 10
	captureAttempts  = 4
	captureBackoff   = 500 * time.Millisecond // doubles, to outlast a brief permission blip
)

// captureGate lets one capture run at a time. Simultaneous ScreenCaptureKit
// requests time out or fail, losing most of their frames. The lock is a file because
// two Argus processes collide just as parallel journeys do.
var captureGate sync.Mutex

func lockCapture() (release func()) {
	captureGate.Lock()
	file, err := os.OpenFile(filepath.Join(os.TempDir(), "argus-capture.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return captureGate.Unlock
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return captureGate.Unlock
	}
	return func() {
		_ = file.Close() // closing drops the lock
		captureGate.Unlock()
	}
}

// Native action names from the accessibility API.
const (
	AXPress = "AXPress"
)

// ControlOptions selects the instance to drive.
type ControlOptions struct {
	Repo     string // Polymux checkout holding scripts/control.sh
	Instance string // exact instance directory printed by scripts/run.sh
}

// ControlError is a refusal from the native helper.
type ControlError struct {
	Code    string
	Summary string
}

func (e *ControlError) Error() string { return e.Code + ": " + e.Summary }

// AXObserver is implemented by harnesses that read the native accessibility
// tree. Native input targets elements, so these replace coordinates.
type AXObserver interface {
	ObserveAX() (screen.AXSnapshot, error)
	Press(key string) error
	Fill(key, text string) error
	Scroll(key, direction string) error
	// Track follows the window until it holds still for quiet, watching at least
	// settle and at most timeout. It returns each distinct state in order.
	Track(quiet, settle, timeout time.Duration) []screen.AXReading
	// Usage samples the subject's CPU percentage and resident memory in KB.
	Usage() (float64, int64, error)
	Scale() float64
	Alive() bool
}

type controlWindow struct {
	Window  string         `json:"window"`
	Capture uint32         `json:"captureWindow"`
	Main    bool           `json:"main"`
	Frame   screen.AXFrame `json:"frame"`
}

type controlReply struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code"`
	Summary string `json:"summary"`
	Effect  string `json:"effect"`
	Target  struct {
		PID int `json:"pid"`
	} `json:"target"`
	Elements []screen.AXElement `json:"elements"`
	Complete bool               `json:"complete"`
	Reasons  []string           `json:"incompleteReasons"`
	Windows  []controlWindow    `json:"windows"`
	Pixels   int                `json:"pixelWidth"`
	// permissions
	Executable    string `json:"executable"`
	Accessibility bool   `json:"accessibility"`
	Recording     bool   `json:"screenRecording"`
}

// ControlHarness implements GUIHarness and AXObserver.
type ControlHarness struct {
	opts    ControlOptions
	binary  string
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	pid     int
	started time.Time

	request sync.Mutex // the session answers one request at a time

	mu     sync.Mutex
	window controlWindow
	scale  float64
	last   *X11Frame
	frames []*X11Frame
	inputs []InputEvent
	seq    int
	shots  int
}

var (
	_ GUIHarness = (*ControlHarness)(nil)
	_ AXObserver = (*ControlHarness)(nil)
)

// StartControl opens a native session on a running instance.
func StartControl(opts ControlOptions) (*ControlHarness, error) {
	binary, err := controlBinary(opts.Repo)
	if err != nil {
		return nil, err
	}
	request, err := json.Marshal(map[string]string{"instance": opts.Instance})
	if err != nil {
		return nil, err
	}
	command := exec.Command(binary, "session", string(request))
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("starting Control: %w", err)
	}
	h := &ControlHarness{
		opts: opts, binary: binary, command: command, stdin: stdin,
		stdout: bufio.NewReader(stdout), started: time.Now(), scale: 1,
	}
	var ready controlReply
	if err := h.read(&ready); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, err
	}
	h.pid = ready.Target.PID
	if _, err := h.ObserveAX(); err != nil {
		_ = h.Close()
		return nil, err
	}
	return h, nil
}

// controlBinary builds the helper if needed and checks it can see the app.
func controlBinary(repo string) (string, error) {
	output, err := exec.Command(filepath.Join(repo, "scripts/control.sh"), "permissions").Output()
	if err != nil {
		return "", fmt.Errorf("Control is unavailable: %w", err)
	}
	var reply controlReply
	if err := json.Unmarshal(output, &reply); err != nil {
		return "", fmt.Errorf("reading Control permissions: %w", err)
	}
	if !reply.Accessibility || !reply.Recording {
		return "", fmt.Errorf("Control needs Accessibility and Screen Recording (accessibility=%t, recording=%t)",
			reply.Accessibility, reply.Recording)
	}
	return reply.Executable, nil
}

func (h *ControlHarness) read(reply *controlReply) error {
	line, err := h.stdout.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("native session ended: %w", err)
	}
	if err := json.Unmarshal(line, reply); err != nil {
		return fmt.Errorf("reading Control reply: %w", err)
	}
	if !reply.OK {
		return &ControlError{Code: reply.Code, Summary: reply.Summary}
	}
	return nil
}

func (h *ControlHarness) send(fields map[string]any, reply *controlReply) error {
	line, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if _, err := h.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("native session ended: %w", err)
	}
	return h.read(reply)
}

// Retryable refusals: the app was busy, which says nothing about its state.
func controlBusy(err error) bool {
	var refusal *ControlError
	if !errors.As(err, &refusal) {
		return false
	}
	switch refusal.Code {
	case "ax_unavailable", "ax_windows_unavailable", "ax_identity_unavailable":
		return true
	}
	return false
}

func (h *ControlHarness) snapshot() (screen.AXSnapshot, error) {
	var reply controlReply
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		reply = controlReply{}
		err = h.send(map[string]any{"op": "snapshot", "maxElements": 5000, "maxDepth": 70}, &reply)
		if err == nil || !controlBusy(err) {
			break
		}
		time.Sleep(controlPoll)
	}
	if err != nil {
		return screen.AXSnapshot{}, err
	}
	snapshot := screen.AXSnapshot{Elements: reply.Elements, Complete: reply.Complete, Reasons: reply.Reasons}
	snapshot.AssignKeys()
	for index, window := range reply.Windows {
		if window.Main || index == 0 {
			snapshot.Window, snapshot.Surface = window.Frame, window.Window
			h.mu.Lock()
			h.window = window
			h.mu.Unlock()
		}
		if window.Main {
			break
		}
	}
	return snapshot, nil
}

// ObserveAX reads every window of the subject.
func (h *ControlHarness) ObserveAX() (screen.AXSnapshot, error) {
	h.request.Lock()
	defer h.request.Unlock()
	return h.snapshot()
}

// act resolves a key on a fresh snapshot, because tokens die with each read.
func (h *ControlHarness) act(key, op string, fields map[string]any) error {
	h.request.Lock()
	defer h.request.Unlock()
	snapshot, err := h.snapshot()
	// An animating view can briefly refuse a complete read.
	for attempt := 0; err == nil && !snapshot.Complete && attempt < controlRetries; attempt++ {
		time.Sleep(2 * controlPoll)
		snapshot, err = h.snapshot()
	}
	if err != nil {
		return err
	}
	if !snapshot.Complete {
		return fmt.Errorf("snapshot incomplete: %v", snapshot.Reasons)
	}
	element, ok := snapshot.Find(key)
	if !ok {
		return fmt.Errorf("element %q is gone", key)
	}
	request := map[string]any{"op": op, "token": element.Token}
	for name, value := range fields {
		request[name] = value
	}
	var reply controlReply
	if err := h.send(request, &reply); err != nil {
		return err
	}
	if reply.Effect == "unknown" {
		return fmt.Errorf("%s on %q had an unknown effect", op, key)
	}
	return nil
}

// Press triggers an element's native press action.
func (h *ControlHarness) Press(key string) error {
	h.recordInput("press", key)
	return h.act(key, "press", nil)
}

// Fill replaces an element's text.
func (h *ControlHarness) Fill(key, text string) error {
	h.recordInput("fill", key)
	return h.act(key, "set", map[string]any{"text": text})
}

// Scroll sends wheel events to an element.
func (h *ControlHarness) Scroll(key, direction string) error {
	h.recordInput("scroll", key)
	return h.act(key, "scroll", map[string]any{"direction": direction, "amount": 10})
}

var controlKeys = map[string]string{
	"ENTER": "return", "ESC": "escape", "TAB": "tab", "SPACE": "space",
	"UP": "up", "DOWN": "down", "LEFT": "left", "RIGHT": "right",
}

// SendKey sends a key to the focused element, or else the main window.
func (h *ControlHarness) SendKey(key string) error {
	name, ok := controlKeys[key]
	if !ok {
		return fmt.Errorf("Control has no key %q", key)
	}
	h.recordInput("key", key)
	snapshot, err := h.ObserveAX()
	if err != nil {
		return err
	}
	target := ""
	for _, element := range snapshot.Elements {
		if element.Depth == 0 && target == "" {
			target = element.Key
		}
		if element.Focused != nil && *element.Focused {
			target = element.Key
		}
	}
	if target == "" {
		return errors.New("no element can take a key")
	}
	return h.act(target, "key", map[string]any{"key": name})
}

// SendText types into the focused element.
func (h *ControlHarness) SendText(text string) error {
	h.recordInput("text", text)
	snapshot, err := h.ObserveAX()
	if err != nil {
		return err
	}
	for _, element := range snapshot.Elements {
		if element.Focused != nil && *element.Focused {
			return h.act(element.Key, "type", map[string]any{"text": text})
		}
	}
	return errors.New("no element has focus")
}

// Click is refused: native input targets elements, not coordinates.
func (h *ControlHarness) Click(x, y int) error {
	return errors.New("Control presses elements; use Press")
}

// ResizeWindow sets the main window's size in points. Control has no resize, and
// changing its source would cost its permissions, so this one call goes to Cua.
func (h *ControlHarness) ResizeWindow(width, height int) error {
	h.recordInput("resize", fmt.Sprintf("%dx%d", width, height))
	h.mu.Lock()
	window := h.window
	h.mu.Unlock()
	request, err := json.Marshal(map[string]any{
		"pid": h.pid, "window_id": window.Capture, "x": window.Frame.X, "y": window.Frame.Y,
		"width": width, "height": height,
	})
	if err != nil {
		return err
	}
	if output, err := exec.Command("cua-driver", "call", "set_window_frame", string(request)).CombinedOutput(); err != nil {
		return fmt.Errorf("resizing: %w: %s", err, bytes.TrimSpace(output))
	}
	return nil
}

// Capture records the main window. The PNG lands in the instance directory,
// which run.sh deletes, because a capture can show private UI.
func (h *ControlHarness) Capture() (*X11Frame, error) {
	h.mu.Lock()
	h.shots++
	output := filepath.Join(h.opts.Instance, fmt.Sprintf("argus-%d.png", h.shots))
	window := h.window
	h.mu.Unlock()
	if window.Capture == 0 {
		return nil, errors.New("the main window has no capture identity")
	}
	defer os.Remove(output)
	reply, err := h.shoot(window.Capture, output)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		return nil, err
	}
	decoded, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	rgba := toRGBAImage(decoded)
	width, height := rgba.Bounds().Dx(), rgba.Bounds().Dy()
	frame := &X11Frame{
		T: time.Since(h.started), Digest: digestBytes(raw), Image: rgba, PNG: raw,
		Width: width, Height: height, TotalPixels: width * height,
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if window.Frame.W > 0 && reply.Pixels > 0 {
		h.scale = float64(reply.Pixels) / window.Frame.W
	}
	frame.ChangedPixels = frame.TotalPixels
	if h.last != nil && h.last.Width == width && h.last.Height == height {
		frame.ChangedPixels = countChangedPixels(h.last.Image, rgba)
	}
	if h.last == nil || frame.Digest != h.last.Digest {
		h.frames = append(h.frames, frame)
		if len(h.frames) > controlMaxFrames {
			h.frames = h.frames[len(h.frames)-controlMaxFrames:]
		}
	}
	h.last = frame
	return frame, nil
}

// LoadPNG reads a saved capture for comparison.
func LoadPNG(path string) (*image.RGBA, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoded, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return toRGBAImage(decoded), nil
}

// shoot writes one window capture, retrying a request the system failed.
func (h *ControlHarness) shoot(window uint32, output string) (controlReply, error) {
	defer lockCapture()()
	request, err := json.Marshal(map[string]any{"instance": h.opts.Instance, "window": window, "output": output})
	if err != nil {
		return controlReply{}, err
	}
	var reply controlReply
	for attempt := 0; attempt < captureAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(captureBackoff << (attempt - 1))
		}
		reply = controlReply{}
		var result []byte
		result, err = exec.Command(h.binary, "capture", string(request)).Output()
		if parsed := json.Unmarshal(result, &reply); parsed != nil && err == nil {
			err = parsed
		}
		if err == nil {
			return reply, nil
		}
		if reply.Summary != "" {
			err = &ControlError{Code: reply.Code, Summary: reply.Summary}
		}
		_ = os.Remove(output)
	}
	return reply, fmt.Errorf("capture failed: %w", err)
}

// Name identifies the harness in reports.
func (h *ControlHarness) Name() string { return "control" }

// Started returns the harness start time.
func (h *ControlHarness) Started() time.Time { return h.started }

// Scale is capture pixels per desktop point.
func (h *ControlHarness) Scale() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.scale
}

// Size returns the main window's size in capture pixels.
func (h *ControlHarness) Size() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return int(h.window.Frame.W * h.scale), int(h.window.Frame.H * h.scale)
}

// Alive reports whether the subject process still exists.
func (h *ControlHarness) Alive() bool { return syscall.Kill(h.pid, 0) == nil }

// Frames returns the retained frames.
func (h *ControlHarness) Frames() []*X11Frame {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*X11Frame{}, h.frames...)
}

// Inputs returns the input trace.
func (h *ControlHarness) Inputs() []InputEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]InputEvent{}, h.inputs...)
}

func (h *ControlHarness) recordInput(kind, data string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	h.inputs = append(h.inputs, InputEvent{T: time.Since(h.started), Seq: h.seq, Kind: kind, Data: data})
}

func (h *ControlHarness) signature() (string, error) {
	snapshot, err := h.ObserveAX()
	if err != nil {
		return "", err
	}
	return snapshot.Signature(), nil
}

// Track implements AXObserver.
func (h *ControlHarness) Track(quiet, settle, timeout time.Duration) []screen.AXReading {
	start := time.Now()
	readings := []screen.AXReading{}
	last, stable := "", start
	for time.Since(start) < timeout {
		began := time.Now()
		snapshot, err := h.ObserveAX()
		if err != nil {
			return readings
		}
		took := time.Since(began)
		if signature := snapshot.Signature(); signature != last {
			readings = append(readings, screen.AXReading{Snapshot: snapshot, At: time.Since(start), Took: took})
			last, stable = signature, time.Now()
		} else {
			if took > readings[len(readings)-1].Took {
				readings[len(readings)-1].Took = took
			}
			if time.Since(stable) >= quiet && time.Since(start) >= settle {
				return readings
			}
		}
		time.Sleep(controlPoll / 4)
	}
	return readings
}

// Usage implements AXObserver.
func (h *ControlHarness) Usage() (float64, int64, error) {
	output, err := exec.Command("ps", "-o", "%cpu=,rss=", "-p", strconv.Itoa(h.pid)).Output()
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected ps output %q", output)
	}
	cpu, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, 0, err
	}
	rss, err := strconv.ParseInt(fields[1], 10, 64)
	return cpu, rss, err
}

// WaitForChange waits for the accessibility tree to change.
func (h *ControlHarness) WaitForChange(timeout time.Duration) bool {
	before, err := h.signature()
	if err != nil {
		return false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(controlPoll)
		if now, err := h.signature(); err == nil && now != before {
			return true
		}
	}
	return false
}

// WaitIdle waits for the accessibility tree to hold still.
func (h *ControlHarness) WaitIdle(quiet, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	previous, _ := h.signature()
	stable := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(controlPoll)
		now, err := h.signature()
		if err != nil {
			return false
		}
		if now != previous {
			previous, stable = now, time.Now()
		} else if time.Since(stable) >= quiet {
			return true
		}
	}
	return false
}

// Observe captures a short run of frames.
func (h *ControlHarness) Observe(count int, interval time.Duration) []*X11Frame {
	out := make([]*X11Frame, 0, count)
	for index := 0; index < count; index++ {
		if frame, err := h.Capture(); err == nil {
			out = append(out, frame)
		}
		time.Sleep(interval)
	}
	return out
}

// Close ends the session; the instance itself belongs to whoever started it.
func (h *ControlHarness) Close() error {
	h.request.Lock()
	var reply controlReply
	_ = h.send(map[string]any{"op": "close"}, &reply)
	h.request.Unlock()
	_ = h.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- h.command.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = h.command.Process.Kill()
		<-done
	}
	return nil
}
