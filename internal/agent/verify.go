package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/carlvincetan/argus/internal/run"
	"github.com/carlvincetan/argus/internal/screen"
)

// Verification is the outcome of a blinded vision check.
type Verification struct {
	Confirmed bool   `json:"confirmed"`
	Note      string `json:"note,omitempty"`
	Model     string `json:"model,omitempty"`
	CallMade  bool   `json:"call_made"`
}

// classKeywords maps a defect class to phrases that would appear in an unbiased
// description of that defect. Matching happens in code, not in the model: the
// vision tier is only asked to describe what it sees, never to agree with a
// hypothesis. That is deliberate -- asking "is this broken?" invites the model
// to say yes.
var classKeywords = map[string][]string{
	"truncation":        {"cut off", "truncat", "clipped", "ellipsis", "cut mid"},
	"duplication":       {"duplicate", "twice", "repeated", "same line", "copied"},
	"misalignment":      {"misalign", "not aligned", "shifted", "ragged", "off by", "uneven column"},
	"border":            {"border", "box", "broken line", "gap in", "missing edge"},
	"readability":       {"hard to read", "low contrast", "faded", "invisible", "dim", "unreadable"},
	"encoding_artifact": {"escape", "garbage", "weird character", "artifact", "stray", "replacement character", "placeholder"},
	"overlap":           {"overlap", "collide", "on top of", "over the"},
	"layout_jitter":     {"moved", "shift", "jitter", "jump", "drift"},
	"animation":         {"flicker", "flashing", "blink", "redraw", "stutter", "clear"},
	"guidance_missing":  {"no way", "cannot", "stuck", "no instruction", "no hint", "dead end"},
	"focus_missing":     {"no selection", "nothing selected", "no highlight", "no focus"},
	"inconsistency":     {"inconsistent", "mixed", "different format", "does not match"},
}

const describePrompt = `This image is a crop of a terminal screen from a program under test.
Describe only what is visible, factually and briefly:
1. The text content, including anything that looks cut off, duplicated, or out of place.
2. The layout: boxes, alignment, spacing, and whether anything overlaps or is drawn over something else.
3. Anything hard to read or visually broken.
Do not speculate about the program's behaviour or intent, and do not judge whether it is good or bad. Just describe.`

// Verify performs the blinded description and decides in code whether the
// description contains evidence for the candidate's defect class.
func (a *Auditor) Verify(
	ctx context.Context,
	finding run.Finding,
	grid *screen.Grid,
	crop *screen.Grid,
) (Verification, error) {
	if a.providers.Vision == nil {
		return Verification{}, fmt.Errorf("no vision provider configured")
	}
	if a.visionCalls >= a.cfg.Limits.MaxVisionCalls {
		return Verification{}, fmt.Errorf("vision call budget exhausted")
	}
	still, err := screen.RenderAnnotated(crop, nil, screen.DefaultRenderOptions())
	if err != nil {
		return Verification{}, err
	}
	description, err := a.providers.Vision.Describe(ctx, [][]byte{still}, describePrompt, map[string]any{
		"purpose": "verify",
		"finding": finding.ID,
	})
	if err != nil {
		return Verification{}, err
	}
	a.visionCalls++
	lower := strings.ToLower(description)
	matched := []string{}
	for _, keyword := range classKeywords[finding.Class] {
		if strings.Contains(lower, keyword) {
			matched = append(matched, keyword)
		}
	}
	// Also treat an explicit mention of the measured detail as support.
	if finding.Check != "" {
		for _, word := range strings.Fields(strings.ToLower(strings.ReplaceAll(finding.Check, "_", " "))) {
			if len(word) > 4 && strings.Contains(lower, word) {
				matched = append(matched, word)
			}
		}
	}
	return Verification{
		Confirmed: len(matched) > 0,
		Note:      fmt.Sprintf("blinded description matched %v", matched),
		Model:     a.providers.Vision.Name(),
		CallMade:  true,
	}, nil
}
