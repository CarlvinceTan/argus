// CDP harness: drives a real browser over the Chrome DevTools Protocol.
//
// It launches headless Chrome, injects input the way a user does (keyboard and
// mouse), resizes the viewport, takes screenshots and reads the DOM. That gives
// the auditor two things the terminal harness cannot: the page's *structure*
// (buttons, links, fields, rows) and pixels for the vision tier.
package harness

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/carlvincetan/argus/internal/screen"
)

// CDPOptions configure a browser session.
type CDPOptions struct {
	Binary      string
	URL         string
	Width       int
	Height      int
	UserDataDir string
	ExtraArgs   []string
	MaxFrames   int
	Timeout     time.Duration
}

// CDPHarness owns a browser process and its protocol connection.
type CDPHarness struct {
	process   *exec.Cmd
	conn      *websocket.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	port      int
	profile   string
	sessionID string
	opts      CDPOptions

	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcReply
	started time.Time
	frames  []*screen.Frame
	last    *screen.Frame
	inputs  []screen.InputEvent
	seq     int
	width   int
	height  int
	console []string
}

type rpcReply struct {
	result json.RawMessage
	err    error
}

// StartCDP launches the browser and connects to it.
func StartCDP(opts CDPOptions) (*CDPHarness, error) {
	binary := opts.Binary
	if binary == "" {
		binary = findBrowser()
	}
	if binary == "" {
		return nil, fmt.Errorf("no Chrome/Chromium binary found; pass --browser")
	}
	if opts.Width <= 0 {
		opts.Width = 1280
	}
	if opts.Height <= 0 {
		opts.Height = 900
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.MaxFrames <= 0 {
		opts.MaxFrames = 400
	}
	profile := opts.UserDataDir
	if profile == "" {
		dir, err := os.MkdirTemp("", "argus-cdp-*")
		if err != nil {
			return nil, err
		}
		profile = dir
	}
	args := []string{
		"--headless=new",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-sync",
		"--metrics-recording-only",
		"--mute-audio",
		"--no-sandbox",
		fmt.Sprintf("--user-data-dir=%s", profile),
		fmt.Sprintf("--window-size=%d,%d", opts.Width, opts.Height),
		"--remote-debugging-port=0",
		"about:blank",
	}
	args = append(args, opts.ExtraArgs...)
	command := exec.Command(binary, args...)
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("starting browser: %w", err)
	}

	port, err := waitForDevToolsPort(profile, opts.Timeout)
	if err != nil {
		_ = command.Process.Kill()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	harness := &CDPHarness{
		process: command,
		ctx:     ctx,
		cancel:  cancel,
		port:    port,
		profile: profile,
		opts:    opts,
		started: time.Now(),
		pending: map[int]chan rpcReply{},
		width:   opts.Width,
		height:  opts.Height,
	}
	if err := harness.connect(); err != nil {
		_ = command.Process.Kill()
		cancel()
		return nil, err
	}
	if opts.URL != "" {
		if err := harness.Navigate(opts.URL); err != nil {
			return nil, err
		}
	}
	return harness, nil
}

// findBrowser looks for a Chrome-family binary in the usual places, including
// the Playwright cache, so a machine set up for browser tests needs no install.
func findBrowser() string {
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
	}
	if home, err := os.UserHomeDir(); err == nil {
		patterns := []string{
			filepath.Join(home, "Library/Caches/ms-playwright/chromium-*/chrome-mac*/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"),
			filepath.Join(home, "Library/Caches/ms-playwright/chromium_headless_shell-*/chrome-headless-shell-mac*/chrome-headless-shell"),
			filepath.Join(home, ".cache/ms-playwright/chromium-*/chrome-linux/chrome"),
		}
		for _, pattern := range patterns {
			if matches, err := filepath.Glob(pattern); err == nil && len(matches) > 0 {
				candidates = append(candidates, matches[len(matches)-1])
			}
		}
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// waitForDevToolsPort reads the port Chrome wrote into its profile.
func waitForDevToolsPort(profile string, timeout time.Duration) (int, error) {
	path := filepath.Join(profile, "DevToolsActivePort")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if len(lines) > 0 {
				if port, err := strconv.Atoi(strings.TrimSpace(lines[0])); err == nil && port > 0 {
					return port, nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, fmt.Errorf("browser did not open a debugging port within %s", timeout)
}

func (h *CDPHarness) connect() error {
	// The browser-level endpoint is enough for page, runtime and input domains
	// once a page target is attached to.
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", h.port))
	if err != nil {
		return fmt.Errorf("reading browser endpoint: %w", err)
	}
	defer response.Body.Close()
	var version struct {
		WebSocket string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(response.Body).Decode(&version); err != nil {
		return fmt.Errorf("decoding browser endpoint: %w", err)
	}
	conn, _, err := websocket.Dial(h.ctx, version.WebSocket, nil)
	if err != nil {
		return fmt.Errorf("connecting to browser: %w", err)
	}
	conn.SetReadLimit(64 << 20)
	h.conn = conn
	go h.readLoop()
	// One page target per session keeps screenshots and DOM reads unambiguous.
	if _, err := h.send("Target.createTarget", map[string]any{"url": "about:blank"}); err != nil {
		return err
	}
	page, err := h.currentPage()
	if err != nil {
		return err
	}
	attached, err := h.send("Target.attachToTarget", map[string]any{"targetId": page, "flatten": true})
	if err != nil {
		return err
	}
	var attachResult struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(attached, &attachResult); err != nil {
		return err
	}
	// With flatten mode, page-domain commands must name the session they belong
	// to; browser-domain commands must not.
	h.sessionID = attachResult.SessionID
	if _, err := h.send("Page.enable", nil); err != nil {
		return err
	}
	if _, err := h.send("Runtime.enable", nil); err != nil {
		return err
	}
	return h.resize(h.width, h.height)
}

// currentPage lists targets and returns the first page target id.
func (h *CDPHarness) currentPage() (string, error) {
	raw, err := h.send("Target.getTargets", nil)
	if err != nil {
		return "", err
	}
	var payload struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	for _, target := range payload.TargetInfos {
		if target.Type == "page" {
			return target.TargetID, nil
		}
	}
	return "", fmt.Errorf("no page target")
}

func (h *CDPHarness) readLoop() {
	for {
		_, data, err := h.conn.Read(h.ctx)
		if err != nil {
			h.mu.Lock()
			for id, channel := range h.pending {
				channel <- rpcReply{err: err}
				delete(h.pending, id)
			}
			h.mu.Unlock()
			return
		}
		var message struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(data, &message); err != nil {
			continue
		}
		if message.Method == "Runtime.consoleAPICalled" {
			var payload struct {
				Args []struct {
					Value any `json:"value"`
				} `json:"args"`
			}
			if json.Unmarshal(message.Params, &payload) == nil {
				line := ""
				for _, argument := range payload.Args {
					line += fmt.Sprintf("%v ", argument.Value)
				}
				h.mu.Lock()
				if len(h.console) < 200 {
					h.console = append(h.console, strings.TrimSpace(line))
				}
				h.mu.Unlock()
			}
			continue
		}
		if message.ID == 0 {
			continue
		}
		h.mu.Lock()
		channel, ok := h.pending[message.ID]
		delete(h.pending, message.ID)
		h.mu.Unlock()
		if !ok {
			continue
		}
		if message.Error != nil {
			channel <- rpcReply{err: fmt.Errorf("%s", message.Error.Message)}
			continue
		}
		channel <- rpcReply{result: message.Result}
	}
}

func (h *CDPHarness) send(method string, params map[string]any) (json.RawMessage, error) {
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	channel := make(chan rpcReply, 1)
	h.pending[id] = channel
	conn := h.conn
	h.mu.Unlock()

	payload := map[string]any{"id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	if h.sessionID != "" && !strings.HasPrefix(method, "Target.") && !strings.HasPrefix(method, "Browser.") {
		payload["sessionId"] = h.sessionID
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	writeCtx, cancel := context.WithTimeout(h.ctx, h.opts.Timeout)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, encoded); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	select {
	case reply := <-channel:
		return reply.result, reply.err
	case <-writeCtx.Done():
		return nil, fmt.Errorf("%s: timed out", method)
	}
}

// Navigate loads a URL and waits for the load event.
func (h *CDPHarness) Navigate(url string) error {
	if _, err := h.send("Page.navigate", map[string]any{"url": url}); err != nil {
		return err
	}
	deadline := time.Now().Add(h.opts.Timeout)
	for time.Now().Before(deadline) {
		raw, err := h.evaluateJS("document.readyState")
		if err == nil && strings.Contains(raw, "complete") {
			time.Sleep(250 * time.Millisecond) // let the first paint settle
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("page did not finish loading: %s", url)
}

// evaluateJS runs a script and returns its value as JSON text.
func (h *CDPHarness) evaluateJS(expression string) (string, error) {
	raw, err := h.send("Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return "", err
	}
	var payload struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	if payload.ExceptionDetails != nil {
		return "", fmt.Errorf("page script error: %s", payload.ExceptionDetails.Text)
	}
	return string(payload.Result.Value), nil
}

// resize sets the viewport, which is how layout defects on the web are exposed.
func (h *CDPHarness) resize(width, height int) error {
	if _, err := h.send("Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             width,
		"height":            height,
		"deviceScaleFactor": 1,
		"mobile":            false,
	}); err != nil {
		return err
	}
	h.mu.Lock()
	h.width, h.height = width, height
	h.mu.Unlock()
	return nil
}

// ResizeWindow satisfies the GUI harness interface.
func (h *CDPHarness) ResizeWindow(width, height int) error { return h.Resize(width, height) }

// Resize changes the viewport and records the input.
func (h *CDPHarness) Resize(width, height int) error {
	h.recordInput("resize", fmt.Sprintf("%dx%d", width, height))
	return h.resize(width, height)
}

// Size returns the viewport size.
func (h *CDPHarness) Size() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.width, h.height
}

// Name labels the harness in logs.
func (h *CDPHarness) Name() string { return fmt.Sprintf("cdp:p%d", h.port) }

// Started returns the harness start time.
func (h *CDPHarness) Started() time.Time { return h.started }

// Capture takes a screenshot and returns it as a frame, recording it when it
// differs from the previous one.
func (h *CDPHarness) Capture() (*screen.Frame, error) {
	raw, err := h.send("Page.captureScreenshot", map[string]any{"format": "png", "captureBeyondViewport": false})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	pngBytes, err := base64.StdEncoding.DecodeString(payload.Data)
	if err != nil {
		return nil, err
	}
	decoded, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, err
	}
	rgba := toRGBAImage(decoded)
	width, height := h.Size()
	frame := &screen.Frame{
		T:             time.Since(h.started),
		Digest:        digestBytes(pngBytes),
		Image:         rgba,
		PNG:           pngBytes,
		Width:         width,
		Height:        height,
		TotalPixels:   width * height,
		ChangedPixels: 0,
	}
	h.mu.Lock()
	if h.last != nil && h.last.Image != nil {
		frame.ChangedPixels = countChangedPixels(h.last.Image, rgba)
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
	h.mu.Unlock()
	return frame, nil
}

// Frames returns the retained changed frames.
func (h *CDPHarness) Frames() []*screen.Frame {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*screen.Frame, len(h.frames))
	copy(out, h.frames)
	return out
}

// Inputs returns the input trace.
func (h *CDPHarness) Inputs() []screen.InputEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]screen.InputEvent, len(h.inputs))
	copy(out, h.inputs)
	return out
}

// Console returns collected console output.
func (h *CDPHarness) Console() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.console...)
}

func (h *CDPHarness) recordInput(kind, data string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	h.inputs = append(h.inputs, screen.InputEvent{T: time.Since(h.started), Seq: h.seq, Kind: kind, Data: data})
}

// SendKey dispatches a single key, mapping the names Argus uses onto DevTools
// key events.
func (h *CDPHarness) SendKey(key string) error {
	upper := strings.ToUpper(strings.TrimSpace(key))
	specials := map[string]struct {
		key  string
		code string
		vk   int
	}{
		"ENTER": {"Enter", "Enter", 13}, "RETURN": {"Enter", "Enter", 13},
		"TAB": {"Tab", "Tab", 9}, "ESC": {"Escape", "Escape", 27}, "ESCAPE": {"Escape", "Escape", 27},
		"SPACE": {" ", "Space", 32}, "BACKSPACE": {"Backspace", "Backspace", 8},
		"DEL": {"Delete", "Delete", 46}, "DELETE": {"Delete", "Delete", 46},
		"UP": {"ArrowUp", "ArrowUp", 38}, "DOWN": {"ArrowDown", "ArrowDown", 40},
		"LEFT": {"ArrowLeft", "ArrowLeft", 37}, "RIGHT": {"ArrowRight", "ArrowRight", 39},
		"HOME": {"Home", "Home", 36}, "END": {"End", "End", 35},
		"PGUP": {"PageUp", "PageUp", 33}, "PGDN": {"PageDown", "PageDown", 34},
		"F1": {"F1", "F1", 112}, "F2": {"F2", "F2", 113}, "F3": {"F3", "F3", 114}, "F4": {"F4", "F4", 115},
	}
	if entry, ok := specials[upper]; ok {
		return h.dispatchKey(entry.key, entry.code, entry.vk, "")
	}
	if strings.HasPrefix(upper, "CTRL+") || strings.HasPrefix(upper, "C-") {
		letter := strings.TrimPrefix(strings.TrimPrefix(upper, "CTRL+"), "C-")
		if len(letter) != 1 {
			return fmt.Errorf("cdp: unsupported control key %q", key)
		}
		return h.dispatchKeyWithModifiers(strings.ToLower(letter), "Key"+letter, int(letter[0]), 2)
	}
	if len([]rune(key)) == 1 {
		char := []rune(key)[0]
		return h.dispatchKey(string(char), keyCodeFor(char), int(char), string(char))
	}
	return fmt.Errorf("cdp: unknown key %q", key)
}

// SendText types literal text into the focused element.
func (h *CDPHarness) SendText(text string) error {
	h.recordInput("text", text)
	if _, err := h.send("Input.insertText", map[string]any{"text": text}); err != nil {
		return err
	}
	return nil
}

func (h *CDPHarness) dispatchKey(key, code string, vk int, text string) error {
	return h.dispatchKeyWithModifiers(key, code, vk, 0)
}

func (h *CDPHarness) dispatchKeyWithModifiers(key, code string, vk int, modifiers int) error {
	h.recordInput("key", key)
	base := map[string]any{
		"key":                   key,
		"code":                  code,
		"windowsVirtualKeyCode": vk,
		"nativeVirtualKeyCode":  vk,
		"modifiers":             modifiers,
	}
	down := map[string]any{"type": "keyDown"}
	for name, value := range base {
		down[name] = value
	}
	if _, err := h.send("Input.dispatchKeyEvent", down); err != nil {
		return err
	}
	up := map[string]any{"type": "keyUp"}
	for name, value := range base {
		up[name] = value
	}
	if _, err := h.send("Input.dispatchKeyEvent", up); err != nil {
		return err
	}
	return nil
}

func keyCodeFor(char rune) string {
	switch {
	case char >= 'a' && char <= 'z':
		return "Key" + strings.ToUpper(string(char))
	case char >= 'A' && char <= 'Z':
		return "Key" + string(char)
	case char >= '0' && char <= '9':
		return "Digit" + string(char)
	}
	switch char {
	case ' ':
		return "Space"
	case '.':
		return "Period"
	case ',':
		return "Comma"
	case '/':
		return "Slash"
	case '-':
		return "Minus"
	case '=':
		return "Equal"
	case ';':
		return "Semicolon"
	case '\'':
		return "Quote"
	case '[':
		return "BracketLeft"
	case ']':
		return "BracketRight"
	case '\\':
		return "Backslash"
	case '`':
		return "Backquote"
	}
	return "Unidentified"
}

// Click clicks at viewport coordinates.
func (h *CDPHarness) Click(x, y int) error {
	h.recordInput("click", fmt.Sprintf("%d,%d", x, y))
	for _, eventType := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		params := map[string]any{"type": eventType, "x": x, "y": y, "button": "left", "clickCount": 1}
		if _, err := h.send("Input.dispatchMouseEvent", params); err != nil {
			return err
		}
	}
	return nil
}

// DOMSnapshot is the page structure the auditor reasons about.
type DOMSnapshot struct {
	URL            string       `json:"url"`
	Title          string       `json:"title"`
	Text           string       `json:"text"`
	Headings       []string     `json:"headings"`
	Elements       []DOMElement `json:"elements"`
	Rows           int          `json:"rows"`
	PasswordFields int          `json:"passwordFields"`
	Forms          int          `json:"forms"`
	Links          []string     `json:"links"`
	Dialogs        int          `json:"dialogs"`
	ErrorTexts     []string     `json:"errorTexts"`
	// ContainerDepth is how many visually distinct containers are nested inside
	// one another, which is the web form of "panels inside panels".
	ContainerDepth int `json:"containerDepth"`
}

// DOMElement is one interactive element on the page.
type DOMElement struct {
	Tag      string `json:"tag"`
	Role     string `json:"role"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Href     string `json:"href"`
	Disabled bool   `json:"disabled"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	W        int    `json:"w"`
	H        int    `json:"h"`
}

// ObserveDOM reads the page's structure and text.
func (h *CDPHarness) ObserveDOM() (DOMSnapshot, error) {
	script := `(() => {
  const visible = el => {
    const rect = el.getBoundingClientRect();
    const style = getComputedStyle(el);
    return rect.width > 1 && rect.height > 1 && style.visibility !== 'hidden' &&
      style.display !== 'none' && Number(style.opacity || 1) > 0.05;
  };
  const nameOf = el => (el.getAttribute('aria-label') || el.getAttribute('title') ||
    el.getAttribute('placeholder') || el.innerText || el.value || el.getAttribute('alt') || '')
    .toString().trim().replace(/\s+/g, ' ').slice(0, 80);
  const selector = 'button,a,input,select,textarea,[role],[onclick],[tabindex]';
  const elements = [...document.querySelectorAll(selector)].filter(visible).slice(0, 300).map(el => {
    const rect = el.getBoundingClientRect();
    return {
      tag: el.tagName.toLowerCase(),
      role: el.getAttribute('role') || '',
      type: el.getAttribute('type') || '',
      name: nameOf(el),
      href: el.getAttribute('href') || '',
      disabled: el.disabled === true || el.getAttribute('aria-disabled') === 'true',
      x: Math.round(rect.x), y: Math.round(rect.y),
      w: Math.round(rect.width), h: Math.round(rect.height)
    };
  });
  return {
    url: location.href,
    title: document.title,
    text: (document.body ? document.body.innerText : '').slice(0, 8000),
    headings: [...document.querySelectorAll('h1,h2,h3,[role=heading]')].filter(visible).map(nameOf).slice(0, 40),
    elements: elements,
    rows: [...document.querySelectorAll('tr,[role=row],li')].filter(visible).length,
    passwordFields: document.querySelectorAll('input[type=password]').length,
    forms: document.forms.length,
    links: [...document.querySelectorAll('a[href]')].filter(visible).map(a => a.getAttribute('href')).slice(0, 100),
    dialogs: document.querySelectorAll('[role=dialog],dialog[open],.modal').length,
    errorTexts: [...document.querySelectorAll('[role=alert],[aria-live],.error,.alert')].filter(visible).map(nameOf).slice(0, 20),
    containerDepth: (() => {
      const isContainer = el => {
        const style = getComputedStyle(el);
        const bordered = style.borderTopWidth !== '0px' || style.borderLeftWidth !== '0px';
        const painted = style.backgroundColor !== 'rgba(0, 0, 0, 0)' && style.boxShadow !== 'none';
        return (bordered || painted) && el.children.length > 0;
      };
      const depthOf = el => {
        let best = 0;
        for (const child of el.children) {
          const nested = isContainer(child) ? 1 + depthOf(child) : depthOf(child);
          if (nested > best) best = nested;
        }
        return best;
      };
      return depthOf(document.body || document.documentElement);
    })()
  };
})()`
	raw, err := h.evaluateJS(script)
	if err != nil {
		return DOMSnapshot{}, err
	}
	var snapshot DOMSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return DOMSnapshot{}, fmt.Errorf("decoding DOM snapshot: %w", err)
	}
	return snapshot, nil
}

// WaitForChange waits until the screenshot differs from the current one.
func (h *CDPHarness) WaitForChange(timeout time.Duration) bool {
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
		time.Sleep(80 * time.Millisecond)
	}
	return false
}

// WaitIdle waits until the page stops changing.
func (h *CDPHarness) WaitIdle(quiet, timeout time.Duration) bool {
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
		time.Sleep(80 * time.Millisecond)
	}
	return false
}

// Observe samples frames at an interval.
func (h *CDPHarness) Observe(count int, interval time.Duration) []*screen.Frame {
	out := make([]*screen.Frame, 0, count)
	for index := 0; index < count; index++ {
		if frame, err := h.Capture(); err == nil {
			out = append(out, frame)
		}
		if index < count-1 {
			time.Sleep(interval)
		}
	}
	return out
}

// Close shuts the browser down and removes its profile.
func (h *CDPHarness) Close() error {
	if h.conn != nil {
		_ = h.conn.Close(websocket.StatusNormalClosure, "done")
	}
	if h.process != nil && h.process.Process != nil {
		_ = h.process.Process.Kill()
	}
	h.cancel()
	if strings.HasPrefix(h.profile, os.TempDir()) {
		_ = os.RemoveAll(h.profile)
	}
	return nil
}

// toRGBAImage converts a decoded image to RGBA.
func toRGBAImage(img image.Image) *image.RGBA {
	bounds := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			out.Set(x, y, img.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return out
}
