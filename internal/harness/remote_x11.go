package harness

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net"
	"sync"
	"time"

	"github.com/carlvincetan/argus/internal/screen"
	"github.com/carlvincetan/argus/internal/sensors"
)

// RemoteX11 talks to an argus-x11-agent running next to the system under test.
//
// It exposes the same operations as the in-process X11 harness, but the pixels
// never cross the network except as a compressed PNG of frames that actually
// changed. The agent keeps the raw frame ledger; the analyser keeps the frames
// it needs for evidence and the change metrics for every capture.
type RemoteX11 struct {
	connection net.Conn
	encoder    *json.Encoder
	decoder    *json.Decoder
	address    string

	mu      sync.Mutex
	started time.Time
	frames  []*X11Frame
	inputs  []InputEvent
	seq     int
	width   int
	height  int
	last    *remoteFrameInfo
}

// remoteEvidence mirrors sensors.Evidence over the wire (regions in pixels).
type remoteEvidence struct {
	Check      string  `json:"check"`
	X0         int     `json:"x0"`
	Y0         int     `json:"y0"`
	X1         int     `json:"x1"`
	Y1         int     `json:"y1"`
	Detail     string  `json:"detail"`
	Severity   float64 `json:"severity_hint"`
	Confidence float64 `json:"confidence"`
	Measured   float64 `json:"measured"`
	GridID     int     `json:"grid_id"`
}

// Evidence converts the wire form into the sensor type.
func (r remoteEvidence) Evidence() sensors.Evidence {
	return sensors.Evidence{
		Check:      r.Check,
		Region:     screen.Region{X0: r.X0, Y0: r.Y0, X1: r.X1, Y1: r.Y1},
		Detail:     r.Detail,
		Severity:   r.Severity,
		Confidence: r.Confidence,
		Measured:   r.Measured,
		GridID:     r.GridID,
	}
}

type remoteFrameInfo struct {
	Digest        string `json:"digest"`
	ChangedPixels int    `json:"changed_pixels"`
	TotalPixels   int    `json:"total_pixels"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	T             int64  `json:"t_ms"`
	PNG           string `json:"png,omitempty"`
}

type remoteResponse struct {
	OK          bool              `json:"ok"`
	Error       string            `json:"error,omitempty"`
	Frame       *remoteFrameInfo  `json:"frame,omitempty"`
	Frames      []remoteFrameInfo `json:"frames,omitempty"`
	Windows     []WindowInfo      `json:"windows,omitempty"`
	Evidence    []remoteEvidence  `json:"evidence,omitempty"`
	ChangedRate float64           `json:"changed_rate,omitempty"`
	Width       int               `json:"width,omitempty"`
	Height      int               `json:"height,omitempty"`
	Note        string            `json:"note,omitempty"`
}

// StartRemoteX11 connects to the agent and discovers the subject window.
func StartRemoteX11(address, windowMatch string) (*RemoteX11, error) {
	connection, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connecting to X11 agent at %s: %w", address, err)
	}
	remote := &RemoteX11{
		connection: connection,
		encoder:    json.NewEncoder(connection),
		decoder:    json.NewDecoder(bufio.NewReader(connection)),
		address:    address,
		started:    time.Now(),
	}
	var reply remoteResponse
	if err := remote.call(map[string]any{"cmd": "size", "window": windowMatch}, &reply); err != nil {
		connection.Close()
		return nil, err
	}
	remote.width, remote.height = reply.Width, reply.Height
	return remote, nil
}

// call sends one request. A dropped tunnel must not end a GUI session, so a
// failed exchange is retried once on a fresh connection (the agent re-resolves
// the subject window when asked).
func (r *RemoteX11) call(request map[string]any, reply *remoteResponse) error {
	command, _ := request["cmd"].(string)
	err := r.exchange(request, reply)
	if err == nil {
		return nil
	}
	if command == "close" {
		return err
	}
	if reconnectErr := r.reconnect(); reconnectErr != nil {
		return fmt.Errorf("%v (reconnect failed: %v)", err, reconnectErr)
	}
	if retryErr := r.exchange(request, reply); retryErr != nil {
		return fmt.Errorf("agent call %v failed after reconnect: %w", command, retryErr)
	}
	return nil
}

func (r *RemoteX11) exchange(request map[string]any, reply *remoteResponse) error {
	if err := r.encoder.Encode(request); err != nil {
		return fmt.Errorf("sending %v: %w", request["cmd"], err)
	}
	if err := r.decoder.Decode(reply); err != nil {
		return fmt.Errorf("reading reply to %v: %w", request["cmd"], err)
	}
	if !reply.OK {
		return fmt.Errorf("agent error for %v: %s", request["cmd"], reply.Error)
	}
	return nil
}

// reconnect re-dials the agent and asks it to re-resolve the subject window.
func (r *RemoteX11) reconnect() error {
	if r.connection != nil {
		_ = r.connection.Close()
	}
	connection, err := net.DialTimeout("tcp", r.address, 10*time.Second)
	if err != nil {
		return err
	}
	r.connection = connection
	r.encoder = json.NewEncoder(connection)
	r.decoder = json.NewDecoder(bufio.NewReader(connection))
	var reply remoteResponse
	if err := r.exchange(map[string]any{"cmd": "refind"}, &reply); err != nil {
		return err
	}
	return nil
}

// Name labels the harness in logs.
func (r *RemoteX11) Name() string { return "remote-x11:" + r.address }

// Started returns the harness start time.
func (r *RemoteX11) Started() time.Time { return r.started }

// Refind calls through for both harness kinds: the session calls it when it has
// just launched the subject.
func (r *RemoteX11) sizeAfterRefind() { r.mu.Lock(); defer r.mu.Unlock() }

// Size returns the subject window size.
func (r *RemoteX11) Size() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.width, r.height
}

// Capture asks the agent for the current frame. The PNG comes back only when the
// frame changed, which is what keeps a remote GUI session affordable.
func (r *RemoteX11) Capture() (*X11Frame, error) {
	var reply remoteResponse
	if err := r.call(map[string]any{"cmd": "capture", "include_png": true}, &reply); err != nil {
		return nil, err
	}
	if reply.Frame == nil {
		return nil, fmt.Errorf("agent returned no frame")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := r.last == nil || reply.Frame.Digest != r.last.Digest
	frame := &X11Frame{
		T:             time.Duration(reply.Frame.T) * time.Millisecond,
		Digest:        reply.Frame.Digest,
		ChangedPixels: reply.Frame.ChangedPixels,
		TotalPixels:   reply.Frame.TotalPixels,
		Width:         reply.Frame.Width,
		Height:        reply.Frame.Height,
	}
	if reply.Frame.PNG != "" {
		raw, err := base64.StdEncoding.DecodeString(reply.Frame.PNG)
		if err == nil {
			frame.PNG = raw
			if decoded, err := png.Decode(bytes.NewReader(raw)); err == nil {
				if rgba, ok := decoded.(*image.RGBA); ok {
					frame.Image = rgba
				} else {
					frame.Image = toRGBA(decoded)
				}
			}
		}
	}
	if frame.Image == nil && r.last != nil && len(r.frames) > 0 {
		// Reuse the previous image so callers always have pixels to look at.
		frame.Image = r.frames[len(r.frames)-1].Image
		frame.PNG = r.frames[len(r.frames)-1].PNG
	}
	r.last = reply.Frame
	r.width, r.height = frame.Width, frame.Height
	if changed {
		r.frames = append(r.frames, frame)
	}
	return frame, nil
}

func toRGBA(img image.Image) *image.RGBA {
	bounds := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			out.Set(x, y, img.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return out
}

// SendKey presses a key on the subject.
func (r *RemoteX11) SendKey(key string) error {
	r.recordInput("key", key)
	return r.call(map[string]any{"cmd": "key", "key": key}, &remoteResponse{})
}

// SendText types a string one character at a time.
func (r *RemoteX11) SendText(text string) error {
	for _, char := range text {
		if err := r.SendKey(string(char)); err != nil {
			return err
		}
	}
	return nil
}

// Click clicks at window-relative coordinates.
func (r *RemoteX11) Click(x, y int) error {
	r.recordInput("click", fmt.Sprintf("%d,%d", x, y))
	return r.call(map[string]any{"cmd": "click", "x": x, "y": y}, &remoteResponse{})
}

// ResizeWindow asks the subject to resize.
func (r *RemoteX11) ResizeWindow(width, height int) error {
	r.recordInput("resize", fmt.Sprintf("%dx%d", width, height))
	var reply remoteResponse
	if err := r.call(map[string]any{"cmd": "resize", "width": width, "height": height}, &reply); err != nil {
		return err
	}
	r.mu.Lock()
	r.width, r.height = width, height
	r.mu.Unlock()
	return nil
}

// Frames returns the retained changed frames.
func (r *RemoteX11) Frames() []*X11Frame {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*X11Frame, len(r.frames))
	copy(out, r.frames)
	return out
}

// Inputs returns the input trace.
func (r *RemoteX11) Inputs() []InputEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]InputEvent, len(r.inputs))
	copy(out, r.inputs)
	return out
}

func (r *RemoteX11) recordInput(kind, data string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.inputs = append(r.inputs, InputEvent{
		T:    time.Since(r.started),
		Seq:  r.seq,
		Kind: kind,
		Data: data,
	})
}

// WaitForChange waits until the agent reports different pixels.
func (r *RemoteX11) WaitForChange(timeout time.Duration) bool {
	r.mu.Lock()
	before := ""
	if r.last != nil {
		before = r.last.Digest
	}
	r.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		frame, err := r.Capture()
		if err == nil && frame.Digest != before {
			return true
		}
		time.Sleep(80 * time.Millisecond)
	}
	return false
}

// WaitIdle waits until the pixels stop changing.
func (r *RemoteX11) WaitIdle(quiet, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	lastChange := time.Now()
	previous := ""
	for time.Now().Before(deadline) {
		frame, err := r.Capture()
		if err != nil {
			return false
		}
		if frame.Digest != previous {
			previous = frame.Digest
			lastChange = time.Now()
		} else if time.Since(lastChange) >= quiet {
			return true
		}
		time.Sleep(80 * time.Millisecond)
	}
	return false
}

// Observe samples frames at an interval.
func (r *RemoteX11) Observe(count int, interval time.Duration) []*X11Frame {
	out := make([]*X11Frame, 0, count)
	for index := 0; index < count; index++ {
		if frame, err := r.Capture(); err == nil {
			out = append(out, frame)
		}
		if index < count-1 {
			time.Sleep(interval)
		}
	}
	return out
}

// Analyze has the agent sample frames locally and run the pixel sensor there.
// A network link cannot sample a 10 Hz animation without aliasing, so the
// measurement happens next to the X server and only its conclusion travels.
func (r *RemoteX11) Analyze(duration, interval time.Duration) ([]sensors.Evidence, []*X11Frame, error) {
	var reply remoteResponse
	request := map[string]any{
		"cmd":         "analyze",
		"duration_ms": duration.Milliseconds(),
		"interval_ms": interval.Milliseconds(),
	}
	if err := r.call(request, &reply); err != nil {
		return nil, nil, err
	}
	evidence := make([]sensors.Evidence, 0, len(reply.Evidence))
	for _, item := range reply.Evidence {
		evidence = append(evidence, item.Evidence())
	}
	frames := []*X11Frame{}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, payload := range reply.Frames {
		frame := &X11Frame{
			T:             time.Duration(payload.T) * time.Millisecond,
			Digest:        payload.Digest,
			ChangedPixels: payload.ChangedPixels,
			TotalPixels:   payload.TotalPixels,
			Width:         payload.Width,
			Height:        payload.Height,
		}
		if payload.PNG != "" {
			raw, err := base64.StdEncoding.DecodeString(payload.PNG)
			if err == nil {
				frame.PNG = raw
				if decoded, err := png.Decode(bytes.NewReader(raw)); err == nil {
					if rgba, ok := decoded.(*image.RGBA); ok {
						frame.Image = rgba
					} else {
						frame.Image = toRGBA(decoded)
					}
				}
			}
		}
		frames = append(frames, frame)
		r.frames = append(r.frames, frame)
	}
	return evidence, frames, nil
}

// Refind asks the agent to re-resolve the subject window.
func (r *RemoteX11) Refind() error {
	var reply remoteResponse
	return r.call(map[string]any{"cmd": "refind"}, &reply)
}

// Close releases the connection.
func (r *RemoteX11) Close() error {
	var reply remoteResponse
	_ = r.call(map[string]any{"cmd": "close"}, &reply)
	return r.connection.Close()
}

// GUIHarness is the interface a GUI session uses, so the same session code runs
// against an in-process X connection or an agent next to the subject.
type GUIHarness interface {
	Name() string
	Started() time.Time
	Size() (int, int)
	Capture() (*X11Frame, error)
	SendKey(key string) error
	SendText(text string) error
	Click(x, y int) error
	ResizeWindow(width, height int) error
	Frames() []*X11Frame
	Inputs() []InputEvent
	WaitForChange(timeout time.Duration) bool
	WaitIdle(quiet, timeout time.Duration) bool
	Observe(count int, interval time.Duration) []*X11Frame
	Close() error
}

// WindowRefinder is implemented by harnesses that can re-resolve their window.
type WindowRefinder interface {
	Refind() error
}

var (
	_ GUIHarness = (*X11Harness)(nil)
	_ GUIHarness = (*RemoteX11)(nil)
)

// RemoteWindows asks an agent which windows exist on its display.
func RemoteWindows(address string) ([]WindowInfo, error) {
	connection, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connecting to X11 agent at %s: %w", address, err)
	}
	defer connection.Close()
	encoder := json.NewEncoder(connection)
	decoder := json.NewDecoder(bufio.NewReader(connection))
	if err := encoder.Encode(map[string]any{"cmd": "windows"}); err != nil {
		return nil, err
	}
	var reply remoteResponse
	if err := decoder.Decode(&reply); err != nil {
		return nil, err
	}
	if !reply.OK {
		return nil, fmt.Errorf("agent error: %s", reply.Error)
	}
	return reply.Windows, nil
}

// OpenGUI connects the right kind of harness for the configuration.
func OpenGUI(address, display, windowMatch string) (GUIHarness, error) {
	if address != "" {
		return StartRemoteX11(address, windowMatch)
	}
	return StartX11(X11Options{Display: display, WindowMatch: windowMatch})
}

// DOMObserver is implemented by harnesses that can read the interface's
// structure rather than only its pixels. The browser harness does; the X11 one
// cannot, which is exactly why the browser path audits web UIs better.
type DOMObserver interface {
	ObserveDOM() (DOMSnapshot, error)
	Navigate(url string) error
}
