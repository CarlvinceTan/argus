// Package knowledge holds the UI judgement knowledge Argus applies.
//
// Two kinds of knowledge live here:
//
//  1. A policy block that goes into the *state* of every decision call. It
//     encodes the expectations a person familiar with terminal and web apps
//     brings without being told: focused lists show a selection, footers
//     advertise the keys that work here, loading states animate, borders are
//     continuous, truncation is visible. Putting the policy in the state (rather
//     than repeating it in every question) is the pattern TypeSafe recommends
//     and it is what lets the model notice unstated-but-obvious problems.
//
//  2. The question bank: atomic, discriminative questions. Broad questions hide
//     several judgments; atomic ones let code weight and combine them, and let a
//     reviewer see exactly which judgement failed.
package knowledge

import (
	"fmt"
	"sort"
	"strings"

	"github.com/carlvincetan/argus/internal/sensors"
)

// Policy is the collated expectation set, included in every audit state.
const Policy = `POLICY: expectations a user brings to a terminal or desktop UI, even when nothing states them.
- Focus and selection: if a list, table, menu or tree is interactive, exactly one row shows as selected/focused. A pane that accepts keys shows how you know which pane it is.
- Key hints: a footer or header advertises the keys that currently work. Hints must match the visible state (no "enter to open" when nothing is selectable, no hint for a pane that is not focused).
- Escape routes: any modal, dialog, overlay or wizard shows how to dismiss or go back. A dead-end screen is a defect even if it never says so.
- Empty and error states: an empty list explains why and what to do next; an error is visually distinct from normal content and says what failed.
- Loading and progress: a busy UI animates (spinner, progress) and the animation advances. A progress bar agrees with its percentage label. Percentages and counters do not go backwards without a reason.
- Layout stability: rows do not jump, jitter, or move horizontally while the screen is idle. The footer stays at the bottom. Panels keep their position between repaints.
- Text integrity: text is either fully visible or visibly truncated (ellipsis). No text is clipped mid-word, overlapped, doubled, or drawn over a border. Wide glyphs (CJK, emoji) keep columns aligned.
- Encoding: no raw escape sequences, replacement characters, printf formatting junk, or placeholder tokens (undefined, NaN) are printed as content.
- Contrast: text is readable against its background, including dimmed or disabled text and text on painted backgrounds.
- Consistency: the same kind of value is formatted the same way everywhere on screen. Alignment is consistent within a block. Wording and casing match surrounding labels.
- Feedback: an action with a visible effect gives feedback within about a second, or shows that it is working.
- Animation: flicker, full-screen clears, full repaints on every keystroke, or partial paints that leave stale content are defects even if the final state looks correct.`

// DefectClasses are the closed set of issue classes used for grouping.
var DefectClasses = map[string]string{
	"none":              "no defect is visible",
	"truncation":        "text or a value is clipped, cut mid-word, or cut off without an ellipsis",
	"duplication":       "the same line, item or block is drawn twice",
	"misalignment":      "columns, borders or rows do not line up with their neighbours",
	"border":            "a box or divider is broken, interrupted, or has content drawn over it",
	"readability":       "text is hard or impossible to read (contrast, invisible text, dim on dim)",
	"encoding_artifact": "raw escapes, replacement characters, format placeholders or encoding damage are shown",
	"overlap":           "elements overlap or text collides with another element",
	"layout_jitter":     "the layout moves, jumps or drifts when it should be stable",
	"animation":         "flicker, full repaints, stuttering animation or a stuck spinner",
	"guidance_missing":  "the screen gives no way to proceed, dismiss, or understand an empty/blocked state",
	"inconsistency":     "formatting, casing, alignment or affordances are inconsistent with the rest of the screen",
	"focus_missing":     "an interactive area shows no selection or focus indicator",
	"other":             "a defect that does not fit the other classes",
	"feature_missing":   "a capability this kind of software should offer is absent",
	"structure":         "content is organised in a way that makes it hard to use",
	"behaviour":         "the software behaves in a way that blocks or misleads the user",
}

// AuditClassCriteria is the class set for the feature/behaviour auditor.
func AuditClassCriteria() map[string]string {
	out := map[string]string{}
	for _, key := range []string{"none", "feature_missing", "structure", "behaviour", "guidance_missing", "inconsistency", "other"} {
		out[key] = DefectClasses[key]
	}
	return out
}

// FeatureCriteria lists the feature vocabulary for "which capability is missing"
// questions, so a completeness answer stays inside the measured vocabulary.
func FeatureCriteria() map[string]string {
	return map[string]string{
		"none":           "nothing important is missing",
		"password_reset": "a way to recover an account",
		"signup":         "a way to create an account",
		"auth":           "any sign-in or credential handling",
		"detail_view":    "a way to inspect a single item",
		"create":         "a way to add something",
		"edit":           "a way to change something",
		"delete":         "a way to remove something",
		"confirmation":   "confirmation before destructive actions",
		"search":         "a way to find items",
		"filter":         "a way to narrow a list",
		"pagination":     "a way to reach items beyond the first screen",
		"settings":       "a way to configure the software",
		"help":           "help or documentation",
		"notifications":  "a way to see messages or alerts",
		"export":         "a way to get data out",
		"empty_state":    "an explanation when there is nothing to show",
		"error_state":    "visible, explained failures",
		"loading_state":  "feedback while work is in progress",
		"sign_out":       "a way to end a session",
		"two_factor":     "second-factor authentication",
		"text_input":     "a way to enter text",
		"navigation":     "a way to move between screens",
	}
}

// SeverityLegend is the ordered impact scale shared by code and model.
var SeverityLegend = []string{
	"cosmetic only, no effect on use",
	"minor annoyance, noticeable but harmless",
	"noticeable: slows the user down or misleads briefly",
	"major: blocks a task or hides required information",
	"critical: data loss, no way out, or a crash",
}

// ScreenQuestions are the always-asked questions about a screen as a whole.
func ScreenQuestions() map[string]string {
	return map[string]string{
		"visible_defect":    "Does `screen.text` show any defect a user would notice? Include clipped text, duplicated rows, misaligned columns, broken borders, unreadable text, overlapping elements, stray escape or format characters, or stale remnants of a previous screen.",
		"defect_class":      "If a defect is visible in `screen.text`, which class fits it best?",
		"severity":          "How much does the most visible problem in `screen.text` affect a user?",
		"blocks_task":       "Would the most visible problem in `screen.text` stop a user from completing the task they came to do?",
		"intended":          "Could the thing that looks wrong in `screen.text` be a deliberate design choice that a reasonable UI would ship, rather than a defect?",
		"unstated":          "Does `screen.text` break an expectation from POLICY that a user would consider obvious even though this screen never states it?",
		"unstated_what":     "If `screen.text` breaks an unstated expectation from POLICY, which class fits that expectation best?",
		"guidance":          "Does `screen.text` show a modal, dialog, empty list, error or blocked state without an available way to dismiss it, proceed, or recover?",
		"focus_missing":     "Does `screen.text` contain an interactive list, table, menu or tree without a visible selected-item or focus indicator?",
		"busy_feedback":     "Does `screen.text` claim work is in progress while the expected moving indicator (spinner, progress, changing counter) is unavailable?",
		"inconsistent":      "Do values that should be formatted alike on `screen.text` look inconsistent with each other in format, casing, alignment or affordance?",
		"unstated_severity": "How much would a user be affected by the unstated expectation from POLICY that `screen.text` breaks?",
		"defect_row":        "Which numbered row of `screen.text` contains the most visible problem? Choose the row where a user would point.",
		"needs_context":     "Is key context absent from `screen.text`, such that a defect judgement could be wrong?",
	}
}

// CandidateQuestions returns the short, atomic questions asked per deterministic
// candidate. Candidate evidence is in the state under `candN`; instructions do
// not interpolate snippets, because those can contain words the decision
// endpoint mistakes for answer labels (e.g. the text "no action available").
func CandidateQuestions() map[string]string {
	return map[string]string{
		"real_defect": "Is this measured candidate a genuine user-visible defect in the supplied screen, rather than an intended design choice?",
		"class":       "Which defect class fits this measured candidate best?",
		"severity":    "How much would this measured candidate affect a user if it is a real defect?",
	}
}

// MotionQuestion translates a motion finding into a question.
func MotionQuestion() string {
	return "Motion finding `motion.detail` was measured across frames (`motion.metrics`). Does this describe a real animation or repaint defect a user would perceive (flicker, stutter, layout jump, full-screen clear, unnecessary redraw), rather than normal animation for this kind of screen?"
}

// RowsCriteria builds the choice criteria for the "which row" question from the
// numbered screen text, so a model-found issue gets a location.
func RowsCriteria(rows []string, limit int) map[string]string {
	criteria := map[string]string{"none": "no single row is the problem"}
	count := 0
	for index, row := range rows {
		trimmed := strings.TrimSpace(row)
		if trimmed == "" {
			continue
		}
		criteria[fmt.Sprintf("%d", index+1)] = truncate(trimmed, 72)
		count++
		if count >= limit {
			break
		}
	}
	return criteria
}

// ClassCriteria maps the class keys to short descriptions for a choice question.
func ClassCriteria() map[string]string {
	return DefectClasses
}

// SeverityScoreLevels returns the ordered severity legend.
func SeverityScoreLevels() []string {
	return append([]string{}, SeverityLegend...)
}

// CandidateSummary renders one deterministic candidate for the model state.
func CandidateSummary(id string, evidence sensors.Evidence) string {
	return fmt.Sprintf("%s: check=%s region=%s severity_hint=%.1f detail=%s snippet=%q",
		id, evidence.Check, evidence.Region, evidence.Severity, evidence.Detail, truncate(evidence.Snippet, 120))
}

// ClassFromCheck maps a deterministic check to a default class, used when the
// model's answer is missing or the candidate never reaches a model call.
func ClassFromCheck(check string) string {
	switch check {
	case sensors.CheckBorderGap, sensors.CheckBorderCrossing:
		return "border"
	case sensors.CheckRaggedPanel, sensors.CheckTableMisalign:
		return "misalignment"
	case sensors.CheckDuplicateLine:
		return "duplication"
	case sensors.CheckHardClip, sensors.CheckEllipsisMidline, sensors.CheckTruncationRoom:
		return "truncation"
	case sensors.CheckInvisibleText, sensors.CheckLowContrast:
		return "readability"
	case sensors.CheckEscapeArtifact, sensors.CheckLiteralJunk:
		return "encoding_artifact"
	case sensors.CheckProgressLabel:
		return "inconsistency"
	case sensors.CheckFlicker, sensors.CheckFullClear, sensors.CheckStuckSpinner:
		return "animation"
	case sensors.CheckFooterDrift, sensors.CheckCadenceJitter, sensors.CheckIdleChurn:
		return "layout_jitter"
	case sensors.AXUnlabelled:
		return "guidance_missing"
	case sensors.AXOverlap:
		return "overlap"
	case sensors.AXClipped, sensors.AXCutText:
		return "truncation"
	case sensors.AXSmallHit:
		return "inconsistency"
	case sensors.AXBlank, sensors.AXInert, sensors.AXCrash, sensors.LogWarning:
		return "behaviour"
	case sensors.DriftAdded, sensors.DriftGone, sensors.DriftMoved, sensors.DriftPixels:
		return "inconsistency"
	case sensors.AXFlash:
		return "animation"
	case sensors.AXShift:
		return "layout_jitter"
	default:
		return "other"
	}
}

// HumanCheckName turns a check identifier into a short phrase for titles.
func HumanCheckName(check string) string {
	switch check {
	case sensors.CheckBorderGap:
		return "broken border"
	case sensors.CheckBorderCrossing:
		return "content over a border"
	case sensors.CheckRaggedPanel:
		return "ragged panel edge"
	case sensors.CheckDuplicateLine:
		return "duplicated line"
	case sensors.CheckHardClip:
		return "clipped text"
	case sensors.CheckEllipsisMidline:
		return "misplaced ellipsis"
	case sensors.CheckTruncationRoom:
		return "value truncated with room to spare"
	case sensors.CheckInvisibleText:
		return "invisible text"
	case sensors.CheckLowContrast:
		return "low contrast text"
	case sensors.CheckEscapeArtifact:
		return "raw escape sequence on screen"
	case sensors.CheckLiteralJunk:
		return "placeholder text on screen"
	case sensors.CheckProgressLabel:
		return "progress bar disagrees with its label"
	case sensors.CheckTableMisalign:
		return "column misalignment"
	case sensors.CheckFlicker:
		return "flicker"
	case sensors.CheckFullClear:
		return "full-screen clear and repaint"
	case sensors.CheckFooterDrift:
		return "layout drift while idle"
	case sensors.CheckStuckSpinner:
		return "stuck spinner"
	case sensors.CheckCadenceJitter:
		return "irregular animation cadence"
	case sensors.CheckIdleChurn:
		return "redraw churn while idle"
	case "screen":
		return "screen-level defect"
	default:
		return strings.ReplaceAll(check, "_", " ")
	}
}

// SortClasses returns class keys in a stable order for prompts and reports.
func SortClasses() []string {
	keys := make([]string, 0, len(DefectClasses))
	for key := range DefectClasses {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func truncate(text string, limit int) string {
	text = strings.ReplaceAll(text, "\n", " ")
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}
