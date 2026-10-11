package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const demoScript = `
printf '\033[2J\033[H'
printf '+--------------------------+\n'
printf '|  Argus demo screen       |\n'
printf '|  status: loading         |\n'
printf '+--------------------------+\n'
printf 'Press any key to continue.\n'
for i in 1 2 3 4 5 6; do
  printf '\rspinner: %s' "$i"
  sleep 0.05
done
printf '\r\033[Kready.            \n'
read -r -s -n 1 key
printf 'you pressed something\n'
sleep 0.1
`

func startDemo(t *testing.T) *Harness {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "demo.sh")
	if err := os.WriteFile(script, []byte(demoScript), 0o755); err != nil {
		t.Fatalf("writing demo script: %v", err)
	}
	h, err := Start(Options{
		Argv: []string{"/bin/sh", script},
		Cols: 80,
		Rows: 24,
	})
	if err != nil {
		t.Fatalf("starting harness: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func TestHarnessCapturesFramesAndText(t *testing.T) {
	h := startDemo(t)
	if !h.WaitIdle(120*time.Millisecond, 5*time.Second) {
		t.Fatal("screen never settled")
	}
	grid := h.Snapshot()
	if got := grid.Row(0); !strings.HasPrefix(got, "+---") {
		t.Fatalf("row 0 = %q, want a box top", got)
	}
	if !strings.Contains(grid.Text(), "Argus demo screen") {
		t.Fatalf("screen text missing title:\n%s", grid.TrimmedText())
	}
	frames := h.Frames()
	if len(frames) < 4 {
		t.Fatalf("expected several frames from the spinner, got %d", len(frames))
	}
	var animated bool
	for _, frame := range frames[1:] {
		if frame.ChangedCells > 0 {
			animated = true
			break
		}
	}
	if !animated {
		t.Fatal("expected changed cells across frames")
	}
}

func TestHarnessSendsInput(t *testing.T) {
	h := startDemo(t)
	if !h.WaitIdle(120*time.Millisecond, 5*time.Second) {
		t.Fatal("screen never settled")
	}
	if err := h.SendKey("ENTER"); err != nil {
		t.Fatalf("send key: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(h.Snapshot().Text(), "you pressed something") {
			inputs := h.Inputs()
			if len(inputs) == 0 || inputs[len(inputs)-1].Kind != "key" {
				t.Fatalf("input trace missing keystroke: %+v", inputs)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("subject never reacted to input:\n%s", h.Snapshot().TrimmedText())
}

func TestHarnessResize(t *testing.T) {
	h := startDemo(t)
	if !h.WaitIdle(120*time.Millisecond, 5*time.Second) {
		t.Fatal("screen never settled")
	}
	if err := h.Resize(50, 12); err != nil {
		t.Fatalf("resize: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	grid := h.Snapshot()
	if grid.Cols != 50 || grid.RowsN != 12 {
		t.Fatalf("grid is %dx%d, want 50x12", grid.Cols, grid.RowsN)
	}
	found := false
	for _, input := range h.Inputs() {
		if input.Kind == "resize" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("resize not recorded in input trace")
	}
}

func TestHarnessSubjectEnvHidesCredentials(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "secret-value")
	env := SubjectEnv()
	for _, entry := range env {
		if strings.HasPrefix(entry, "OPENROUTER_API_KEY=") {
			t.Fatalf("subject environment leaked credentials: %s", entry)
		}
	}
}

func TestEncodeKey(t *testing.T) {
	cases := map[string][]byte{
		"ENTER":  {'\r'},
		"UP":     {0x1b, '[', 'A'},
		"CTRL+C": {3},
		"a":      {'a'},
		"ALT+x":  {0x1b, 'x'},
	}
	for key, want := range cases {
		got, err := EncodeKey(key)
		if err != nil {
			t.Fatalf("EncodeKey(%q): %v", key, err)
		}
		if string(got) != string(want) {
			t.Fatalf("EncodeKey(%q) = %q, want %q", key, got, want)
		}
	}
	if _, err := EncodeKey("NOPE+NOPE"); err == nil {
		t.Fatal("expected unknown key to error")
	}
}
