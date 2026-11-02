package provider

import (
	"strings"
	"testing"

	"github.com/carlvincetan/argus/internal/jev"
)

func TestLocalPromptKeepsEveryKeyOfANonScreenState(t *testing.T) {
	prompt := buildLocalPrompt(map[string]any{
		"task":     "decide",
		"change":   map[string]any{"diff": "+Text(\"new\")"},
		"journeys": map[string]any{"journey_00": "chat"},
	}, map[string]jev.Question{"journey_00": jev.Noul("Affected?")})
	for _, want := range []string{"STATE:", "TASK: \"decide\"", `CHANGE: {"diff":"+Text(\"new\")"}`, "JOURNEYS:", "journey_00 [noul] Affected?"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %s:\n%s", want, prompt)
		}
	}
	screen := buildLocalPrompt(map[string]any{"screen": map[string]any{"text": "hello"}, "extra": 1}, map[string]jev.Question{"q": jev.Noul("ok?")})
	if !strings.HasPrefix(screen, "SCREEN:\nhello\n") || strings.Contains(screen, "EXTRA") {
		t.Errorf("a screen prompt changed:\n%s", screen)
	}
}
