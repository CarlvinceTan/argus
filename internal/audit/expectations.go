package audit

import (
	"fmt"
	"sort"
	"strings"

	"github.com/carlvincetan/argus/internal/screen"
)

// Expectation encodes the knowledge a person brings to software: when an
// application offers one feature, users expect its companions. It is deliberately
// a small, auditable table rather than a model prompt, because a model asked
// "should this app have X?" says yes to almost anything.
type Expectation struct {
	When      Feature
	Expect    Feature
	Rationale string
	Severity  float64
	// MinRows gates expectations that only make sense at scale, for example
	// pagination on a list long enough to need it.
	MinRows int
}

// Expectations is the companion-feature table.
func Expectations() []Expectation {
	return []Expectation{
		{
			When:      Auth,
			Expect:    PasswordReset,
			Rationale: "a sign-in screen with no way to recover an account leaves a locked-out user with no path at all",
			Severity:  3.0,
		},
		{
			When:      Auth,
			Expect:    ErrorState,
			Rationale: "authentication fails for ordinary reasons (wrong password, expired key) and the user must be told what happened",
			Severity:  2.5,
		},
		{
			When:      Auth,
			Expect:    TwoFactor,
			Rationale: "an account guarded by a password usually offers a second factor, or at least mentions one",
			Severity:  1.0,
		},
		{
			When:      List,
			Expect:    Detail,
			Rationale: "a list whose rows cannot be opened gives the user no way to see or act on an individual item",
			Severity:  2.5,
		},
		{
			When:      List,
			Expect:    EmptyState,
			Rationale: "a list that can be empty must say so, otherwise it looks broken",
			Severity:  2.0,
		},
		{
			When:      List,
			Expect:    Search,
			Rationale: "a list long enough to scroll needs a way to find a specific item",
			Severity:  1.5,
			MinRows:   12,
		},
		{
			When:      List,
			Expect:    Pagination,
			Rationale: "a list longer than a screen needs a way to reach the rest",
			Severity:  1.5,
			MinRows:   20,
		},
		{
			When:      Detail,
			Expect:    Navigation,
			Rationale: "a detail view has to offer a way back to where the user came from",
			Severity:  2.0,
		},
		{
			When:      Create,
			Expect:    ErrorState,
			Rationale: "creating something can fail validation and the user must see why",
			Severity:  2.0,
		},
		{
			When:      Create,
			Expect:    Confirm,
			Rationale: "creating something usually ends in visible confirmation that it happened",
			Severity:  1.5,
		},
		{
			When:      Delete,
			Expect:    Confirm,
			Rationale: "destructive actions need confirmation, because there is no undo",
			Severity:  3.0,
		},
		{
			When:      Search,
			Expect:    EmptyState,
			Rationale: "a search that finds nothing must say so, or the user keeps retyping",
			Severity:  2.0,
		},
		{
			When:      Settings,
			Expect:    Help,
			Rationale: "settings screens need an explanation of what the options do",
			Severity:  1.0,
		},
		{
			When:      Notifications,
			Expect:    EmptyState,
			Rationale: "an inbox that can be empty must say so",
			Severity:  1.5,
		},
	}
}

// Gap is a measured absence: a feature the application does not offer although
// something it does offer implies it should.
type Gap struct {
	ID         string     `json:"id"`
	Missing    Feature    `json:"missing"`
	Trigger    Feature    `json:"trigger"`
	Rationale  string     `json:"rationale"`
	Severity   float64    `json:"severity"`
	Confidence float64    `json:"confidence"`
	Evidence   []Evidence `json:"evidence"`
}

// Candidate is anything the auditor wants the decision tier to judge: a missing
// feature, a structural problem, or a behavioural one.
type Candidate struct {
	ID         string        `json:"id"`
	Kind       string        `json:"kind"` // feature_gap | structure | behaviour
	Title      string        `json:"title"`
	Detail     string        `json:"detail"`
	Region     screen.Region `json:"region"`
	Severity   float64       `json:"severity"`
	Confidence float64       `json:"confidence"`
	Missing    []Feature     `json:"missing,omitempty"`
	Evidence   []Evidence    `json:"evidence,omitempty"`
}

// Gaps derives the missing-feature candidates from an inventory.
func Gaps(inventory *Inventory) []Gap {
	if inventory == nil {
		return nil
	}
	out := []Gap{}
	for _, expectation := range Expectations() {
		if !inventory.Has(expectation.When) {
			continue
		}
		if inventory.Has(expectation.Expect) {
			continue
		}
		if expectation.MinRows > 0 && maxListRows(inventory) < expectation.MinRows {
			continue
		}
		gap := Gap{
			ID:         fmt.Sprintf("gap:%s-without-%s", expectation.When, expectation.Expect),
			Missing:    expectation.Expect,
			Trigger:    expectation.When,
			Rationale:  expectation.Rationale,
			Severity:   expectation.Severity,
			Confidence: 0.6,
			Evidence:   inventory.Hits[expectation.When],
		}
		out = append(out, gap)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Severity > out[b].Severity })
	return out
}

// Candidates converts gaps and structural measurements into reviewable items.
func Candidates(inventory *Inventory) []Candidate {
	if inventory == nil {
		return nil
	}
	out := []Candidate{}
	for _, gap := range Gaps(inventory) {
		where := ""
		if len(gap.Evidence) > 0 {
			where = " (observed on " + gap.Evidence[0].Screen + ")"
		}
		out = append(out, Candidate{
			ID:         gap.ID,
			Kind:       "feature_gap",
			Title:      fmt.Sprintf("%s exists but %s does not", gap.Trigger, gap.Missing),
			Detail:     gap.Rationale + where,
			Region:     regionOf(gap.Evidence),
			Severity:   gap.Severity,
			Confidence: gap.Confidence,
			Missing:    []Feature{gap.Missing},
			Evidence:   gap.Evidence,
		})
	}

	// Structural: nesting the user has to read through.
	if inventory.Structure.MaxNesting >= 3 {
		out = append(out, Candidate{
			ID:         "structure:nesting",
			Kind:       "structure",
			Title:      fmt.Sprintf("panels are nested %d deep", inventory.Structure.MaxNesting),
			Detail:     "content is drawn inside boxes inside boxes inside boxes, so the user has to read through three frames to reach the actual content",
			Severity:   2.0,
			Confidence: 0.7,
		})
	}

	// Behavioural: advertised controls that did nothing when tried.
	for _, inert := range inventory.Structure.InertAffordances {
		out = append(out, Candidate{
			ID:         "behaviour:inert-affordance",
			Kind:       "behaviour",
			Title:      fmt.Sprintf("advertised control %q does nothing", strings.TrimSpace(inert.Label)),
			Detail:     fmt.Sprintf("the interface advertises %q on %s and using it %d time(s) produced no visible change", inert.Label, inert.Screen, inert.Tried),
			Severity:   2.5,
			Confidence: 0.8,
		})
	}

	// Behavioural: screens that only offer the way out.
	for _, screenName := range inventory.Structure.DeadEndScreens {
		out = append(out, Candidate{
			ID:         "behaviour:dead-end:" + screenName,
			Kind:       "behaviour",
			Title:      fmt.Sprintf("%s offers no way forward", cleanScreenName(screenName)),
			Detail:     "every advertised control on this screen leaves it; nothing moves the task forward",
			Severity:   2.0,
			Confidence: 0.55,
		})
	}

	// Structure: the same value repeated across panels.
	for _, repeated := range inventory.Structure.RepeatedValues {
		out = append(out, Candidate{
			ID:         "structure:repeated-value",
			Kind:       "structure",
			Title:      fmt.Sprintf("the value %q is shown %d times", repeated.Value, repeated.Count),
			Detail:     "the same information appears repeatedly on one screen, which usually means two panels are describing the same thing",
			Severity:   1.5,
			Confidence: 0.5,
		})
	}
	return out
}

// ObserveStructure records the per-screen structural measurements into the
// inventory. It is separate from feature hits because these need whole-screen
// geometry rather than text.
func (i *Inventory) ObserveStructure(grid *screen.Grid, screenName string) {
	if grid == nil {
		return
	}
	if depth, region := nestingDepth(grid); depth > i.Structure.MaxNesting {
		i.Structure.MaxNesting = depth
		i.Structure.NestingScreens = append(i.Structure.NestingScreens, fmt.Sprintf("%s (depth %d, %s)", screenName, depth, region))
	}
	if selections := competingSelections(grid); selections > i.Structure.FocusRegions {
		i.Structure.FocusRegions = selections
	}
	if repeated := repeatedValues(grid, 8); len(repeated) > 0 {
		i.Structure.RepeatedValues = append(i.Structure.RepeatedValues, repeated...)
		if len(i.Structure.RepeatedValues) > 6 {
			i.Structure.RepeatedValues = i.Structure.RepeatedValues[:6]
		}
	}
	if deadEnd(grid) {
		if !containsString(i.Structure.DeadEndScreens, screenName) {
			i.Structure.DeadEndScreens = append(i.Structure.DeadEndScreens, screenName)
		}
	}
}

// RecordInertAffordance notes a control that was tried and changed nothing.
func (i *Inventory) RecordInertAffordance(label, screenName string, tried int) {
	label = strings.TrimSpace(label)
	if label == "" {
		return
	}
	for index := range i.Structure.InertAffordances {
		if i.Structure.InertAffordances[index].Label == label && i.Structure.InertAffordances[index].Screen == screenName {
			i.Structure.InertAffordances[index].Tried += tried
			return
		}
	}
	i.Structure.InertAffordances = append(i.Structure.InertAffordances, InertAffordance{
		Label:  label,
		Screen: screenName,
		Tried:  max(1, tried),
	})
}

func maxListRows(inventory *Inventory) int {
	best := 0
	for _, shape := range inventory.Lists {
		if shape.Rows > best {
			best = shape.Rows
		}
	}
	return best
}

// cleanScreenName strips box drawing and decoration from a screen label, so a
// finding does not read "┌─ Sign in ──┐ offers no way forward".
func cleanScreenName(name string) string {
	trimmed := strings.TrimSpace(strings.Trim(strings.TrimSpace(name), "┌┐└┘│┃║─━═|"))
	if trimmed == "" {
		return name
	}
	return trimmed
}

func regionOf(evidence []Evidence) screen.Region {
	for _, entry := range evidence {
		if entry.Region.Width() > 0 || entry.Region.Height() > 0 {
			return entry.Region
		}
	}
	return screen.Region{X0: 0, Y0: 0, X1: 0, Y1: 0}
}
