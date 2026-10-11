package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/jev"
	"github.com/carlvincetan/argus/internal/provider"
	"github.com/carlvincetan/argus/internal/screen"
)

// Action kinds.
const (
	ActionKey    = "key"
	ActionText   = "text"
	ActionResize = "resize"
	ActionRedraw = "redraw"
	ActionIdle   = "idle"
	ActionDone   = "done"
)

// Action is one thing the explorer can do to the subject.
type Action struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Key    string `json:"key,omitempty"`
	Text   string `json:"text,omitempty"`
	Size   [2]int `json:"size,omitempty"`
	Label  string `json:"label"`
	Source string `json:"source"`
}

// Destructive labels we refuse to click or press, because a test run must not
// destroy the subject's state (or the user's data).
var destructiveWords = []string{
	"quit", "exit", "delete", "remove", "kill", "destroy", "drop", "format",
	"reset", "purge", "erase", "uninstall", "shutdown", "abort", "discard",
	"overwrite", "clear all", "wipe",
}

func isDestructive(label string) bool {
	lower := strings.ToLower(label)
	for _, word := range destructiveWords {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// navigationKeys are always offered: they are how TUIs are explored.
var navigationKeys = []string{"DOWN", "UP", "RIGHT", "LEFT", "TAB", "SHIFT+TAB", "ENTER", "ESC", "PGDN", "PGUP", "SPACE"}

// probeKeys are safe-ish single keys that commonly reveal help, search or list
// navigation in terminal applications.
var probeKeys = []string{"?", "/", "h", "j", "k"}

// sampleTexts are the values the explorer may type into a focused input.
var sampleTexts = []string{"test", "argus", "123", "query", "hello world"}

// resizeSizes are the layouts the explorer probes; layout bugs show up on resize.
var resizeSizes = [][2]int{{80, 24}, {120, 40}, {60, 18}, {100, 30}}

// BuildActionSpace derives the indexed action space for a screen: advertised key
// hints first, then navigation, then safe probes, then layout and observation
// operations. The index order is stable so a model answer maps back to exactly
// one action.
func BuildActionSpace(grid *screen.Grid, allowText bool) []Action {
	actions := []Action{}
	seen := map[string]bool{}
	add := func(action Action) {
		if seen[action.ID] || isDestructive(action.Label) {
			return
		}
		seen[action.ID] = true
		actions = append(actions, action)
	}
	for _, hint := range grid.Structure().KeyHints {
		key := normalizeKey(hint.Key)
		if key == "" {
			continue
		}
		add(Action{
			ID:     "hint:" + strings.ToUpper(key),
			Kind:   ActionKey,
			Key:    key,
			Label:  fmt.Sprintf("press %s (%s)", key, hint.Label),
			Source: "hint",
		})
	}
	for _, key := range navigationKeys {
		add(Action{
			ID:     "nav:" + key,
			Kind:   ActionKey,
			Key:    key,
			Label:  fmt.Sprintf("press %s", key),
			Source: "navigation",
		})
	}
	for _, key := range probeKeys {
		add(Action{
			ID:     "probe:" + key,
			Kind:   ActionKey,
			Key:    key,
			Label:  fmt.Sprintf("press %s", key),
			Source: "probe",
		})
	}
	for _, highlight := range grid.Structure().Highlights {
		if len(strings.TrimSpace(highlight.Text)) < 3 {
			continue
		}
		add(Action{
			ID:     "activate:" + highlight.Region.String(),
			Kind:   ActionKey,
			Key:    "ENTER",
			Label:  fmt.Sprintf("activate the highlighted item %q", truncateLabel(highlight.Text)),
			Source: "highlight",
		})
	}
	for index, text := range sampleTexts {
		if !allowText {
			break
		}
		add(Action{
			ID:     fmt.Sprintf("text:%d", index),
			Kind:   ActionText,
			Text:   text,
			Label:  fmt.Sprintf("type %q into the focused field", text),
			Source: "input",
		})
	}
	for _, size := range resizeSizes {
		add(Action{
			ID:     fmt.Sprintf("resize:%dx%d", size[0], size[1]),
			Kind:   ActionResize,
			Size:   size,
			Label:  fmt.Sprintf("resize the terminal to %dx%d", size[0], size[1]),
			Source: "layout",
		})
	}
	add(Action{ID: "redraw", Kind: ActionRedraw, Label: "force a redraw with Ctrl+L", Source: "layout"})
	add(Action{ID: "idle", Kind: ActionIdle, Label: "wait and watch for animation problems", Source: "animation"})
	return actions
}

func normalizeKey(raw string) string {
	key := strings.TrimSpace(raw)
	upper := strings.ToUpper(key)
	switch upper {
	case "", "SEARCH", "NEXT", "PREV", "BACK", "SELECT", "OPEN", "TOGGLE":
		return ""
	}
	if strings.HasPrefix(upper, "^") {
		letter := strings.TrimPrefix(upper, "^")
		if letter == "?" {
			return "CTRL+?"
		}
		return "CTRL+" + letter
	}
	if strings.HasPrefix(upper, "C-") {
		return "CTRL+" + strings.TrimPrefix(upper, "C-")
	}
	if strings.HasPrefix(upper, "CTRL+") {
		return upper
	}
	if len([]rune(key)) == 1 || upper == "ESC" || upper == "ENTER" || upper == "TAB" || upper == "SPACE" {
		return key
	}
	switch upper {
	case "UP", "DOWN", "LEFT", "RIGHT", "PGUP", "PGDN":
		return upper
	}
	// Interfaces often draw arrow glyphs in their hint bars.
	switch key {
	case "↑":
		return "UP"
	case "↓":
		return "DOWN"
	case "←":
		return "LEFT"
	case "→":
		return "RIGHT"
	}
	return ""
}

func truncateLabel(text string) string {
	cleaned := strings.TrimSpace(text)
	runes := []rune(cleaned)
	if len(runes) <= 40 {
		return cleaned
	}
	return string(runes[:40]) + "..."
}

// Explorer chooses the next action, using the decision tier as the policy and
// falling back to deterministic probes when the model is unavailable or the
// screen has stopped changing.
type Explorer struct {
	cfg       config.Config
	providers provider.Set

	attempted   map[string]int
	history     []string
	steps       int
	calls       int
	inputTokens int
	costUSD     float64
	stuckCount  int
	exhausted   map[string]bool
	allowText   bool
}

// AllowTextInput enables TYPE_TEXT operations. Submitting text changes program
// state (it usually triggers a request or a command), so it is off by default:
// a UI defect hunt should not need a working backend to explore the interface.
func (e *Explorer) AllowTextInput(enabled bool) {
	e.allowText = enabled
}

// NewExplorer creates an explorer for a session.
func NewExplorer(cfg config.Config, providers provider.Set) *Explorer {
	return &Explorer{
		cfg:       cfg,
		providers: providers,
		attempted: map[string]int{},
		exhausted: map[string]bool{},
	}
}

// History returns the recent action labels.
func (e *Explorer) History() []string {
	if len(e.history) > 10 {
		return e.history[len(e.history)-10:]
	}
	return append([]string{}, e.history...)
}

// Usage reports explorer decision usage.
func (e *Explorer) Usage() (calls, tokens int, cost float64) {
	return e.calls, e.inputTokens, e.costUSD
}

// Next picks an action for the current screen.
func (e *Explorer) Next(ctx context.Context, grid *screen.Grid, subject string) (Action, bool) {
	space := BuildActionSpace(grid, e.allowText)
	stateKey := grid.Digest
	fresh := make([]Action, 0, len(space))
	for _, action := range space {
		key := stateKey + "|" + action.ID
		if e.attempted[key] > 0 {
			continue
		}
		fresh = append(fresh, action)
	}
	if len(fresh) == 0 {
		// Everything on this screen was tried: force a layout or redraw probe
		// that is not tied to this screen's content.
		for _, action := range space {
			if action.Kind == ActionRedraw || action.Kind == ActionIdle || action.Kind == ActionResize {
				return action, true
			}
		}
		return Action{ID: "done", Kind: ActionDone, Label: "nothing left to try"}, false
	}

	action, ok := e.askModel(ctx, grid, subject, fresh)
	if !ok {
		action = fresh[0]
	}
	e.mark(action, stateKey)
	return action, true
}

// MarkAttempted records an action as attempted for the current screen so the
// explorer never repeats a dead end.
func (e *Explorer) MarkAttempted(grid *screen.Grid, action Action) {
	e.mark(action, grid.Digest)
	e.steps++
	e.history = append(e.history, action.Label)
}

func (e *Explorer) mark(action Action, stateKey string) {
	e.attempted[stateKey+"|"+action.ID]++
}

func (e *Explorer) askModel(ctx context.Context, grid *screen.Grid, subject string, fresh []Action) (Action, bool) {
	if e.calls >= e.cfg.Limits.MaxDecisions {
		return Action{}, false
	}
	operations := map[string]string{}
	targets := map[string][]Action{}
	for index, action := range fresh {
		operation := operationFor(action)
		operations[operation] = operationDescription(operation)
		targets[operation] = append(targets[operation], action)
		_ = index
	}
	operations["DONE"] = "No offered action can reveal anything new or useful; stop exploring this screen."
	criteria := map[string]string{}
	for operation, description := range operations {
		criteria[operation] = description
	}
	questions := map[string]jev.Question{
		"operation": jev.Choice(exploreInstruction(subject), criteria),
	}
	for operation, actions := range targets {
		labelToAction := map[string]string{}
		options := map[string]string{}
		for _, action := range actions {
			options[action.ID] = action.Label
			labelToAction[action.ID] = action.Label
		}
		questions[operation+"_target"] = jev.Choice(targetInstruction(operation), options)
	}
	state := map[string]any{
		"app":     map[string]any{"name": subject, "screen": screen.ScreenName(grid)},
		"screen":  map[string]any{"text": numbered(strings.Split(grid.TrimmedText(), "\n"), 40), "structure": grid.SummaryForModel()},
		"history": e.history,
		"policy":  explorePolicy,
	}
	decision, err := e.providers.Decisions.Decide(ctx, state, questions, 1, map[string]any{"purpose": "explore"})
	if err != nil {
		return Action{}, false
	}
	e.calls++
	e.inputTokens += decision.Usage.InputTokens
	e.costUSD += decision.Usage.CostUSD
	operationAnswer, ok := decision.Answers["operation"]
	if !ok {
		return Action{}, false
	}
	operation := operationAnswer.Choice
	if operation == "DONE" {
		return Action{ID: "done", Kind: ActionDone, Label: "model chose to stop"}, false
	}
	candidates := targets[operation]
	if len(candidates) == 0 {
		return Action{}, false
	}
	targetAnswer, ok := decision.Answers[operation+"_target"]
	if !ok {
		return candidates[0], true
	}
	for _, action := range candidates {
		if action.ID == targetAnswer.Choice {
			return action, true
		}
	}
	return candidates[0], true
}

func operationFor(action Action) string {
	switch action.Kind {
	case ActionResize:
		return "RESIZE"
	case ActionRedraw:
		return "REDRAW"
	case ActionIdle:
		return "OBSERVE"
	case ActionText:
		return "TYPE_TEXT"
	default:
		if action.Source == "navigation" {
			return "NAVIGATE"
		}
		return "PRESS_KEY"
	}
}

func operationDescription(operation string) string {
	switch operation {
	case "PRESS_KEY":
		return "Press a key that the interface itself advertises in a hint, to open a menu, dialog or view it mentions."
	case "NAVIGATE":
		return "Move the selection with an arrow, tab, page or enter key to reveal neighbouring items and states."
	case "TYPE_TEXT":
		return "Type a plausible value into the focused input field, which may reveal validation, filtering or result states."
	case "RESIZE":
		return "Change the terminal size, which is how layout, wrapping and clipping defects become visible."
	case "REDRAW":
		return "Force the application to repaint, which reveals leftover or stale content."
	case "OBSERVE":
		return "Wait without sending input, which reveals animation, flicker and idle-repaint problems."
	default:
		return operation
	}
}

func exploreInstruction(subject string) string {
	return fmt.Sprintf(`Advance the exploration of the terminal application %q so that latent user-visible defects surface.
Choose the single operation most likely to reveal a state nobody has looked at yet: an unopened menu, an unfiltered list, an empty state, an error path, a dialog, or a layout that has not been resized.
Prefer advertised controls over guessing. Prefer operations that change what is on screen. Never choose an operation whose label suggests quitting, deleting, resetting or destroying data.
Use the recent action history to avoid repeating a dead end. Choose DONE only when every offered operation is a repeat or pointless.`, subject)
}

func targetInstruction(operation string) string {
	return fmt.Sprintf(`Choose the target for the %s operation. Pick the one most likely to reveal an unexplored state, not the safest or most familiar one.`, operation)
}

const explorePolicy = `EXPLORATION POLICY
- A tester's job is coverage: open every menu, enter every mode, scroll every list, resize the window, and wait in loading states.
- Prefer controls the application advertises (key hints) but also move the selection: many states only appear after navigating.
- Avoid destructive labels: quit, exit, delete, remove, reset, kill, overwrite.
- A state already seen is worth less than an unseen one; the history shows what was already tried.
- Screens that never change are suspicious: prefer OBSERVE or RESIZE when the history looks repetitive.`

// SortActions returns actions in a deterministic order (used by tests and logs).
func SortActions(actions []Action) []Action {
	out := append([]Action{}, actions...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
