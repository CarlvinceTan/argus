// Personas: the "army of testers" idea made concrete.
//
// One reviewer asking thirty questions answers all of them shallowly. Argus runs
// several narrow reviewers over the same evidence instead, each with its own
// slice of the question bank and its own instructions, and merges what they find.
// The personas are deliberately specialised the way people are: somebody looks at
// rendering, somebody at behaviour, somebody at what is simply missing.
package knowledge

// Persona is one specialist reviewer.
type Persona struct {
	ID    string
	Title string
	Focus string
	// ScreenQuestions are the ids from ScreenQuestions this persona asks about a
	// terminal screen. A nil slice means "all of them".
	ScreenQuestions []string
	// Checks are the deterministic check ids this persona owns. A nil slice
	// means "every check that fired".
	Checks []string
	// Motion decides whether the persona reviews animation evidence.
	Motion bool
	// Completeness marks the persona that runs the feature-gap audit.
	Completeness bool
}

// Personas returns the reviewer roster.
func Personas() []Persona {
	return []Persona{
		{
			ID:              "visual",
			Title:           "Rendering reviewer",
			Focus:           "You look at how the interface is drawn: text that is cut off, boxes that do not close, columns that do not line up, elements that overlap, colours that cannot be read, stray escape or placeholder characters.",
			ScreenQuestions: []string{"visible_defect", "defect_class", "severity", "intended", "defect_row", "inconsistent"},
			Checks: []string{
				"border_gap", "border_crossing", "ragged_panel", "duplicate_line", "hard_clip",
				"ellipsis_midline", "truncation_with_room", "invisible_text", "low_contrast",
				"escape_artifact", "literal_junk", "table_misalignment", "progress_label_mismatch",
				"column", "text",
			},
			Motion: true,
		},
		{
			ID:              "behaviour",
			Title:           "Behaviour reviewer",
			Focus:           "You look at how the software behaves over time and across states: screens that only let you leave, controls that advertise an action and do nothing, information repeated in three panels, layouts that jump while idle, flows that dead-end.",
			ScreenQuestions: []string{"unstated", "unstated_what", "unstated_severity", "guidance", "focus_missing", "busy_feedback", "needs_context"},
			Checks:          []string{"structure", "behaviour", "layout", "drift", "churn", "cadence", "spinner", "gui_"},
			Motion:          true,
		},
		{
			ID:              "animation",
			Title:           "Animation reviewer",
			Focus:           "You look only at movement: whether animation is smooth, regular and meaningful, whether anything flickers, blinks without conveying state, or repaints while nothing is happening.",
			ScreenQuestions: []string{"busy_feedback"},
			Checks:          []string{"flicker", "clear", "cadence", "churn", "drift", "spinner", "gui_"},
			Motion:          true,
		},
		{
			ID:           "completeness",
			Title:        "Completeness reviewer",
			Focus:        "You look at what the software does not offer. You are given a measured inventory of features that exist; you judge whether the companions of those features are missing (account recovery for a login, a detail view for a list, confirmation for a destructive action), and you never claim a feature is missing when the inventory shows any form of it.",
			Motion:       false,
			Completeness: true,
		},
		{
			ID:              "errors",
			Title:           "Failure-path reviewer",
			Focus:           "You look at what happens when things go wrong: whether failures are visible and explained, whether there is a way to retry, whether error text is distinguishable from normal content, and whether an operation can be started whose outcome the user cannot see.",
			ScreenQuestions: []string{"guidance", "needs_context", "visible_defect", "severity"},
			Checks:          []string{"error", "escape", "literal"},
			Motion:          false,
		},
	}
}

// PersonaByID finds a persona.
func PersonaByID(id string) (Persona, bool) {
	for _, persona := range Personas() {
		if persona.ID == id {
			return persona, true
		}
	}
	return Persona{}, false
}

// DefaultPersonaIDs is the set a run uses when none is chosen: the three that
// cover rendering, behaviour and completeness. Animation and failure paths are
// available but skipped by default to keep a run affordable.
func DefaultPersonaIDs() []string {
	return []string{"visual", "behaviour", "completeness"}
}

// ResolvePersonas maps ids to personas, falling back to the defaults when the
// list is empty and reporting unknown ids.
func ResolvePersonas(ids []string) ([]Persona, []string) {
	if len(ids) == 0 {
		ids = DefaultPersonaIDs()
	}
	out := []Persona{}
	unknown := []string{}
	for _, id := range ids {
		persona, ok := PersonaByID(id)
		if !ok {
			unknown = append(unknown, id)
			continue
		}
		out = append(out, persona)
	}
	return out, unknown
}

// WantsQuestion reports whether a persona asks a given screen question.
func (p Persona) WantsQuestion(id string) bool {
	if p.Completeness {
		return false
	}
	if p.ScreenQuestions == nil {
		return true
	}
	for _, candidate := range p.ScreenQuestions {
		if candidate == id {
			return true
		}
	}
	return false
}

// WantsCheck reports whether a persona owns a deterministic check.
func (p Persona) WantsCheck(check string) bool {
	if p.Completeness {
		return false
	}
	if p.Checks == nil {
		return true
	}
	for _, prefix := range p.Checks {
		if prefix == check || (len(check) >= len(prefix) && check[:len(prefix)] == prefix) {
			return true
		}
	}
	return false
}
