// X11 harness: drives and observes a real GUI program over the X protocol.
//
// It connects directly to an X server (a local one, or a remote Xvfb over a
// tunnel), finds the subject's top-level window, uploads nothing, injects real
// input with XTEST, and captures the window as pixels. That makes a GUI subject
// observable in exactly the way a user sees it: pixels over time, with no
// cooperation from the program under test.
package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/png"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgb/xtest"

	"github.com/carlvincetan/argus/internal/screen"
)

// X11Options configure a GUI session.
type X11Options struct {
	Display     string // e.g. "localhost:99"
	WindowMatch string // WM_NAME substring; empty picks the largest child window
	MaxFrames   int
}

// X11Frame is one captured GUI screen (see screen.Frame).
type X11Frame = screen.Frame

// X11Harness owns one X11 connection and the window it tests.
type X11Harness struct {
	conn   *xgb.Conn
	root   xproto.Window
	window xproto.Window
	gc     xproto.Gcontext
	opts   X11Options

	mu       sync.Mutex
	width    int
	height   int
	started  time.Time
	frames   []*X11Frame
	last     *X11Frame
	keycodes map[xproto.Keysym]byte
	shift    byte

	inputs []InputEvent
	seq    int
}

// StartX11 connects to the display and locates the subject window.
func StartX11(opts X11Options) (*X11Harness, error) {
	if opts.Display == "" {
		opts.Display = ":99"
	}
	if opts.MaxFrames <= 0 {
		opts.MaxFrames = 600
	}
	conn, err := xgb.NewConnDisplay(opts.Display)
	if err != nil {
		return nil, fmt.Errorf("connecting to X display %s: %w", opts.Display, err)
	}
	if err := xtest.Init(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("X server has no XTEST extension: %w", err)
	}
	setup := xproto.Setup(conn)
	screen := setup.DefaultScreen(conn)

	harness := &X11Harness{
		conn:     conn,
		root:     screen.Root,
		opts:     opts,
		started:  time.Now(),
		keycodes: map[xproto.Keysym]byte{},
	}
	if err := harness.loadKeyboard(); err != nil {
		conn.Close()
		return nil, err
	}
	window, err := harness.findWindow(opts.WindowMatch)
	if err != nil {
		conn.Close()
		return nil, err
	}
	harness.window = window
	geometry, err := xproto.GetGeometry(conn, xproto.Drawable(window)).Reply()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading window geometry: %w", err)
	}
	harness.width = int(geometry.Width)
	harness.height = int(geometry.Height)
	return harness, nil
}

// findWindow waits for a suitable top-level window and returns it.
//
// A long-lived X server accumulates windows from previous runs, all with the
// same name and size, and a dead one looks exactly like a live one until you
// look at the pixels. So candidates are ranked by stacking order and then probed:
// the first window whose pixels actually change wins, and a static window is
// accepted only if nothing else moves.
func (h *X11Harness) findWindow(match string) (xproto.Window, error) {
	deadline := time.Now().Add(15 * time.Second)
	for {
		candidates := h.windowCandidates(match)
		if len(candidates) > 0 {
			if choice, ok := h.pickLiveWindow(candidates); ok {
				return choice, nil
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("no suitable window appeared on %s (looked for %q)", h.opts.Display, match)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// windowCandidates lists viewable top-level windows, topmost last.
func (h *X11Harness) windowCandidates(match string) []xproto.Window {
	children, err := xproto.QueryTree(h.conn, h.root).Reply()
	if err != nil {
		return nil
	}
	out := []xproto.Window{}
	for _, child := range children.Children {
		attributes, err := xproto.GetWindowAttributes(h.conn, child).Reply()
		if err != nil || attributes.MapState != xproto.MapStateViewable {
			continue
		}
		geometry, err := xproto.GetGeometry(h.conn, xproto.Drawable(child)).Reply()
		if err != nil {
			continue
		}
		area := int(geometry.Width) * int(geometry.Height)
		name := h.windowName(child)
		if match != "" {
			if !strings.Contains(name, match) {
				continue
			}
		} else if name == "" && area < 200*200 {
			continue
		}
		out = append(out, child)
	}
	return out
}

// pickLiveWindow returns the topmost candidate that shows activity.
func (h *X11Harness) pickLiveWindow(candidates []xproto.Window) (xproto.Window, bool) {
	for index := len(candidates) - 1; index >= 0; index-- {
		window := candidates[index]
		first, err := h.readDigest(window)
		if err != nil {
			continue
		}
		time.Sleep(160 * time.Millisecond)
		second, err := h.readDigest(window)
		if err != nil {
			continue
		}
		if first != second {
			h.window = window
			return window, true
		}
	}
	if len(candidates) > 0 {
		// Nothing is animating: take the topmost window rather than failing.
		h.window = candidates[len(candidates)-1]
		return h.window, true
	}
	return 0, false
}

// readDigest captures a window far enough to hash it.
func (h *X11Harness) readDigest(window xproto.Window) (string, error) {
	geometry, err := xproto.GetGeometry(h.conn, xproto.Drawable(window)).Reply()
	if err != nil {
		return "", err
	}
	width, height := int(geometry.Width), int(geometry.Height)
	if width == 0 || height == 0 {
		return "", fmt.Errorf("empty window")
	}
	reply, err := xproto.GetImage(
		h.conn, xproto.ImageFormatZPixmap, xproto.Drawable(window),
		0, 0, uint16(width), uint16(height), ^uint32(0),
	).Reply()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(reply.Data)
	return hex.EncodeToString(sum[:8]), nil
}

func (h *X11Harness) windowName(window xproto.Window) string {
	reply, err := xproto.GetProperty(h.conn, false, window, xproto.AtomWmName, xproto.AtomString, 0, 256).Reply()
	if err != nil {
		return ""
	}
	return string(reply.Value)
}

// loadKeyboard builds a keysym -> keycode map from the server.
func (h *X11Harness) loadKeyboard() error {
	setup := xproto.Setup(h.conn)
	min, max := setup.MinKeycode, setup.MaxKeycode
	reply, err := xproto.GetKeyboardMapping(h.conn, min, byte(max-min+1)).Reply()
	if err != nil {
		return fmt.Errorf("reading keyboard mapping: %w", err)
	}
	perCode := int(reply.KeysymsPerKeycode)
	for index, keysym := range reply.Keysyms {
		if keysym == 0 {
			continue
		}
		keycode := byte(int(min) + index/perCode)
		if _, exists := h.keycodes[keysym]; !exists {
			h.keycodes[keysym] = keycode
		}
	}
	h.shift = h.keycodes[0xffe1] // Shift_L
	if h.shift == 0 {
		return fmt.Errorf("X server exposes no Shift key")
	}
	return nil
}

// Refind re-resolves the subject window. A session that relaunches its subject
// must call this: the previous window is gone, and a harness still attached to it
// sees a perfectly static screen.
func (h *X11Harness) Refind() error {
	window, err := h.findWindow(h.opts.WindowMatch)
	if err != nil {
		return err
	}
	h.window = window
	geometry, err := xproto.GetGeometry(h.conn, xproto.Drawable(window)).Reply()
	if err == nil {
		h.mu.Lock()
		h.width, h.height = int(geometry.Width), int(geometry.Height)
		h.mu.Unlock()
	}
	h.mu.Lock()
	h.frames = nil
	h.last = nil
	h.mu.Unlock()
	return nil
}

// Name returns a label for logs.
func (h *X11Harness) Name() string { return fmt.Sprintf("x11:%s", h.opts.Display) }

// Started returns the harness start time.
func (h *X11Harness) Started() time.Time { return h.started }

// Capture reads the window pixels and records a frame if anything changed.
func (h *X11Harness) Capture() (*X11Frame, error) {
	geometry, err := xproto.GetGeometry(h.conn, xproto.Drawable(h.window)).Reply()
	if err != nil {
		return nil, fmt.Errorf("reading window geometry: %w", err)
	}
	width, height := int(geometry.Width), int(geometry.Height)
	if width == 0 || height == 0 {
		return nil, fmt.Errorf("window has no size")
	}
	image, err := h.getImage(width, height)
	if err != nil {
		return nil, err
	}
	pngBytes, err := encodePNG(image)
	if err != nil {
		return nil, err
	}
	frame := &X11Frame{
		T:           time.Since(h.started),
		Digest:      digestBytes(pngBytes),
		Image:       image,
		PNG:         pngBytes,
		Width:       width,
		Height:      height,
		TotalPixels: width * height,
	}
	h.mu.Lock()
	if h.last != nil && h.last.Width == width && h.last.Height == height {
		frame.ChangedPixels = countChangedPixels(h.last.Image, image)
	} else {
		frame.ChangedPixels = frame.TotalPixels
	}
	changed := h.last == nil || frame.Digest != h.last.Digest
	if changed {
		h.frames = append(h.frames, frame)
		if len(h.frames) > h.opts.MaxFrames {
			h.frames = h.frames[len(h.frames)-h.opts.MaxFrames:]
		}
	}
	h.last = frame
	h.width, h.height = width, height
	h.mu.Unlock()
	return frame, nil
}

// getImage pulls the window contents with GetImage and converts them to RGBA.
func (h *X11Harness) getImage(width, height int) (*image.RGBA, error) {
	reply, err := xproto.GetImage(
		h.conn, xproto.ImageFormatZPixmap, xproto.Drawable(h.window),
		0, 0, uint16(width), uint16(height), ^uint32(0),
	).Reply()
	if err != nil {
		return nil, fmt.Errorf("reading window pixels: %w", err)
	}
	if reply.Depth != 24 && reply.Depth != 32 {
		return nil, fmt.Errorf("unsupported X depth %d (only 24 and 32 are handled)", reply.Depth)
	}
	data := reply.Data
	if len(data) < width*height*4 {
		return nil, fmt.Errorf("short pixel reply: %d bytes for %dx%d", len(data), width, height)
	}
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	stride := width * 4
	for y := 0; y < height; y++ {
		source := y * stride
		row := out.Pix[y*out.Stride : y*out.Stride+width*4]
		for x := 0; x < width; x++ {
			offset := source + x*4
			row[x*4+0] = data[offset+2] // R
			row[x*4+1] = data[offset+1] // G
			row[x*4+2] = data[offset+0] // B
			row[x*4+3] = 0xff
		}
	}
	return out, nil
}

func encodePNG(img image.Image) ([]byte, error) {
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		return nil, fmt.Errorf("encoding screenshot: %w", err)
	}
	return buffer.Bytes(), nil
}

func digestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

func countChangedPixels(a, b *image.RGBA) int {
	if a == nil || b == nil || a.Bounds() != b.Bounds() {
		return 0
	}
	changed := 0
	for y := 0; y < a.Bounds().Dy(); y++ {
		rowA := a.Pix[y*a.Stride : y*a.Stride+a.Bounds().Dx()*4]
		rowB := b.Pix[y*b.Stride : y*b.Stride+b.Bounds().Dx()*4]
		for x := 0; x < len(rowA); x += 4 {
			if rowA[x] != rowB[x] || rowA[x+1] != rowB[x+1] || rowA[x+2] != rowB[x+2] {
				changed++
			}
		}
	}
	return changed
}

// keySyms maps the keystrokes Argus uses onto X keysyms, with a shift flag for
// characters that need Shift held.
var keySyms = map[string]struct {
	keysym xproto.Keysym
	shift  bool
}{
	"ENTER": {0xff0d, false}, "RETURN": {0xff0d, false},
	"TAB": {0xff09, false}, "SHIFT+TAB": {0xff09, true},
	"ESC": {0xff1b, false}, "ESCAPE": {0xff1b, false},
	"SPACE": {0x20, false}, "BACKSPACE": {0xff08, false},
	"DEL": {0xffff, false}, "DELETE": {0xffff, false},
	"UP": {0xff52, false}, "DOWN": {0xff54, false},
	"LEFT": {0xff51, false}, "RIGHT": {0xff53, false},
	"HOME": {0xff50, false}, "END": {0xff57, false},
	"PGUP": {0xff55, false}, "PGDN": {0xff56, false},
	"F1": {0xffbe, false}, "F2": {0xffbf, false}, "F3": {0xffc0, false}, "F4": {0xffc1, false},
	"F5": {0xffc2, false}, "F6": {0xffc3, false}, "F7": {0xffc4, false}, "F8": {0xffc5, false},
}

// shiftedSymbols are the ASCII characters whose keysym differs from the base key.
var shiftedSymbols = map[rune]rune{
	'!': '1', '@': '2', '#': '3', '$': '4', '%': '5', '^': '6', '&': '7', '*': '8',
	'(': '9', ')': '0', '_': '-', '+': '=', '{': '[', '}': ']', '|': '\\',
	':': ';', '"': '\'', '<': ',', '>': '.', '?': '/', '~': '`',
}

// SendKey presses and releases one key.
func (h *X11Harness) SendKey(key string) error {
	upper := strings.ToUpper(strings.TrimSpace(key))
	if entry, ok := keySyms[upper]; ok {
		return h.tap(entry.keysym, entry.shift)
	}
	if strings.HasPrefix(upper, "CTRL+") || strings.HasPrefix(upper, "C-") {
		letter := strings.TrimPrefix(strings.TrimPrefix(upper, "CTRL+"), "C-")
		if len(letter) != 1 {
			return fmt.Errorf("x11: unsupported control key %q", key)
		}
		control := h.keycodes[0xffe3] // Control_L
		if control == 0 {
			return fmt.Errorf("x11: no Control key on this server")
		}
		code, ok := h.keycodes[xproto.Keysym(strings.ToLower(letter)[0])]
		if !ok {
			return fmt.Errorf("x11: no keycode for %q", key)
		}
		h.recordInput("key", key)
		if err := h.fakeInput(xproto.KeyPress, control, 0, 0); err != nil {
			return err
		}
		if err := h.fakeInput(xproto.KeyPress, code, 0, 0); err != nil {
			return err
		}
		if err := h.fakeInput(xproto.KeyRelease, code, 0, 0); err != nil {
			return err
		}
		return h.fakeInput(xproto.KeyRelease, control, 0, 0)
	}
	if len([]rune(key)) != 1 {
		return fmt.Errorf("x11: unknown key %q", key)
	}
	char := []rune(key)[0]
	base := char
	shifted := false
	if substituted, ok := shiftedSymbols[char]; ok {
		base, shifted = substituted, true
	} else if char >= 'A' && char <= 'Z' {
		base, shifted = char+32, true
	}
	code, ok := h.keycodes[xproto.Keysym(base)]
	if !ok {
		return fmt.Errorf("x11: no keycode for %q", key)
	}
	return h.tapWithCode(code, shifted)
}

func (h *X11Harness) tap(keysym xproto.Keysym, shift bool) error {
	code, ok := h.keycodes[keysym]
	if !ok {
		return fmt.Errorf("x11: keysym %#x is not on this server's keyboard", keysym)
	}
	return h.tapWithCode(code, shift)
}

func (h *X11Harness) tapWithCode(code byte, shift bool) error {
	h.recordInput("key", fmt.Sprintf("keycode=%d", code))
	if shift {
		if err := h.fakeInput(xproto.KeyPress, h.shift, 0, 0); err != nil {
			return err
		}
	}
	if err := h.fakeInput(xproto.KeyPress, code, 0, 0); err != nil {
		return err
	}
	if err := h.fakeInput(xproto.KeyRelease, code, 0, 0); err != nil {
		return err
	}
	if shift {
		return h.fakeInput(xproto.KeyRelease, h.shift, 0, 0)
	}
	return nil
}

// SendText types a string one character at a time.
func (h *X11Harness) SendText(text string) error {
	for _, char := range text {
		if err := h.SendKey(string(char)); err != nil {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// Click moves the pointer and clicks at window-relative coordinates.
func (h *X11Harness) Click(x, y int) error {
	h.recordInput("click", fmt.Sprintf("%d,%d", x, y))
	if err := h.fakeInput(xproto.MotionNotify, 0, x, y); err != nil {
		return err
	}
	if err := h.fakeInput(xproto.ButtonPress, 1, x, y); err != nil {
		return err
	}
	return h.fakeInput(xproto.ButtonRelease, 1, x, y)
}

func (h *X11Harness) fakeInput(eventType byte, detail byte, rootX, rootY int) error {
	return xtest.FakeInputChecked(h.conn, eventType, detail, 0, h.root, int16(rootX), int16(rootY), 0).Check()
}

// ResizeWindow asks the subject to resize, which exercises its layout code.
func (h *X11Harness) ResizeWindow(width, height int) error {
	h.recordInput("resize", fmt.Sprintf("%dx%d", width, height))
	return xproto.ConfigureWindowChecked(h.conn, h.window,
		xproto.ConfigWindowWidth|xproto.ConfigWindowHeight,
		[]uint32{uint32(width), uint32(height)},
	).Check()
}

// Size returns the current window size in pixels.
func (h *X11Harness) Size() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.width, h.height
}

// Frames returns the retained frame ledger.
func (h *X11Harness) Frames() []*X11Frame {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*X11Frame, len(h.frames))
	copy(out, h.frames)
	return out
}

// Inputs returns the input trace.
func (h *X11Harness) Inputs() []InputEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]InputEvent, len(h.inputs))
	copy(out, h.inputs)
	return out
}

func (h *X11Harness) recordInput(kind, data string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	h.inputs = append(h.inputs, InputEvent{
		T:    time.Since(h.started),
		Seq:  h.seq,
		Kind: kind,
		Data: data,
	})
}

// WaitForChange waits for the window pixels to differ from the current frame.
func (h *X11Harness) WaitForChange(timeout time.Duration) bool {
	h.mu.Lock()
	before := ""
	if h.last != nil {
		before = h.last.Digest
	}
	h.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		frame, err := h.Capture()
		if err == nil && frame.Digest != before {
			return true
		}
		time.Sleep(60 * time.Millisecond)
	}
	return false
}

// WaitIdle waits until the pixels stop changing for the quiet period.
func (h *X11Harness) WaitIdle(quiet, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	lastChange := time.Now()
	previous := ""
	for time.Now().Before(deadline) {
		frame, err := h.Capture()
		if err != nil {
			return false
		}
		if frame.Digest != previous {
			previous = frame.Digest
			lastChange = time.Now()
		} else if time.Since(lastChange) >= quiet {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// Observe samples frames at a fixed interval without sending input.
func (h *X11Harness) Observe(count int, interval time.Duration) []*X11Frame {
	out := make([]*X11Frame, 0, count)
	for index := 0; index < count; index++ {
		frame, err := h.Capture()
		if err == nil {
			out = append(out, frame)
		}
		if index < count-1 {
			time.Sleep(interval)
		}
	}
	return out
}

// Close releases the X connection.
func (h *X11Harness) Close() error {
	h.conn.Close()
	return nil
}

// WindowInfo describes a candidate window on the display, for diagnostics.
type WindowInfo struct {
	ID       uint32 `json:"id"`
	Name     string `json:"name"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	MapState byte   `json:"map_state"`
	Mapped   bool   `json:"mapped"`
}

// ListWindows lists the root's children so a GUI setup can be checked.
func ListWindows(display string) ([]WindowInfo, error) {
	conn, err := xgb.NewConnDisplay(display)
	if err != nil {
		return nil, fmt.Errorf("connecting to X display %s: %w", display, err)
	}
	defer conn.Close()
	screen := xproto.Setup(conn).DefaultScreen(conn)
	children, err := xproto.QueryTree(conn, screen.Root).Reply()
	if err != nil {
		return nil, fmt.Errorf("querying the window tree: %w", err)
	}
	out := []WindowInfo{}
	for _, child := range children.Children {
		attributes, err := xproto.GetWindowAttributes(conn, child).Reply()
		if err != nil {
			continue
		}
		geometry, err := xproto.GetGeometry(conn, xproto.Drawable(child)).Reply()
		if err != nil {
			continue
		}
		name := ""
		if reply, err := xproto.GetProperty(conn, false, child, xproto.AtomWmName, xproto.AtomString, 0, 256).Reply(); err == nil {
			name = string(reply.Value)
		}
		out = append(out, WindowInfo{
			ID:       uint32(child),
			Name:     name,
			Width:    int(geometry.Width),
			Height:   int(geometry.Height),
			MapState: attributes.MapState,
			Mapped:   attributes.MapState == xproto.MapStateViewable,
		})
	}
	return out, nil
}
