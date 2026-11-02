package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/carlvincetan/argus/internal/sensors"
)

// ReplayStep is one action from an earlier run and what it led to.
type ReplayStep struct {
	Action    guiAction `json:"action"`
	Signature string    `json:"signature"`
	Digest    string    `json:"digest"`
	Scene     string    `json:"scene"`
	// State is what the window held after this step.
	State *sensors.State `json:"state,omitempty"`
}

// Replay is the action trace of a run. Replaying it spends no model call on
// choosing actions, and none on a screen identical to the recorded one.
type Replay struct {
	Start string         `json:"start"` // signature before the first step
	Scene string         `json:"scene"`
	State *sensors.State `json:"state,omitempty"`
	Steps []ReplayStep   `json:"steps"`
	// Approved and Reason are set when a person makes this trace a baseline.
	Approved string `json:"approved,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Mermaid draws the states a run visited and the actions between them, so that a
// reviewer can judge the flow itself rather than single screens.
func (r *Replay) Mermaid() string {
	var text strings.Builder
	text.WriteString("stateDiagram-v2\n")
	names := map[string]string{}
	name := func(signature, scene string) string {
		if id, ok := names[signature]; ok {
			return id
		}
		id := fmt.Sprintf("s%d", len(names))
		names[signature] = id
		fmt.Fprintf(&text, "    %s: %s\n", id, mermaidText(scene))
		return id
	}
	from := name(r.Start, r.Scene)
	fmt.Fprintf(&text, "    [*] --> %s\n", from)
	seen := map[string]bool{}
	for _, step := range r.Steps {
		to := name(step.Signature, step.Scene)
		if edge := from + ">" + to + ">" + step.Action.ID; to != from && !seen[edge] {
			seen[edge] = true
			fmt.Fprintf(&text, "    %s --> %s: %s\n", from, to, mermaidText(step.Action.Label))
		}
		from = to
	}
	return text.String()
}

// mermaidText keeps a label to characters a state diagram can hold.
func mermaidText(label string) string {
	label = strings.NewReplacer(":", " ", "\"", "", "\n", " ", "#", "", ";", " ").Replace(label)
	if runes := []rune(label); len(runes) > 48 {
		label = string(runes[:48]) + "..."
	}
	return label
}

// LoadReplay reads a trace; a journey with no trace yet returns nil.
func LoadReplay(path string) (*Replay, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var replay Replay
	if err := json.Unmarshal(raw, &replay); err != nil {
		return nil, fmt.Errorf("reading replay %s: %w", path, err)
	}
	return &replay, nil
}

// Save writes the trace atomically, so a killed run leaves the old one intact.
func (r *Replay) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
