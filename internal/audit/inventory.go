// Package audit turns observations of an application into a feature inventory,
// and the inventory into gaps: features that are missing, unreachable or
// structurally wrong given what the application already has.
//
// This is the "auditor" half of Argus. The visual checks answer "is this screen
// rendered correctly"; this package answers "is this software complete, and does
// it behave like the thing it claims to be". Both end up as findings, but the
// evidence is different: a visual finding points at pixels, a completeness
// finding points at what the application does and does not offer.
//
// Division of labour, which the user asked to have decided explicitly:
//
//   - Code (this package) determines what exists. Feature hits, reachability,
//     nesting depth, dead ends and inert affordances are measured, never guessed,
//     because a model asked "does this app have a forgot-password flow?" will
//     invent an answer.
//   - The decision tier (Jev, or the open-source calibrated model) judges what
//     the measured absence means for this application, how severe it is, and
//     whether the expectation is reasonable here. That is a judgement call, and
//     it benefits from calibration and review history.
//   - The LLM/vision tier supplies semantics code cannot see: what a screen is
//     for, what an unlabelled control does, whether two panels are the same
//     information presented twice.
package audit

import (
	"fmt"
	"sort"
	"strings"

	"github.com/carlvincetan/argus/internal/screen"
)

// Feature is a coarse capability a user would recognise by name.
type Feature string

// The feature vocabulary. It is deliberately small and boring: a taxonomy nobody
// can remember is a taxonomy nobody can review.
const (
	Auth          Feature = "auth"
	PasswordReset Feature = "password_reset"
	Signup        Feature = "signup"
	TwoFactor     Feature = "two_factor"
	SessionEnd    Feature = "sign_out"
	List          Feature = "list"
	Detail        Feature = "detail_view"
	Create        Feature = "create"
	Edit          Feature = "edit"
	Delete        Feature = "delete"
	Confirm       Feature = "confirmation"
	Search        Feature = "search"
	Filter        Feature = "filter"
	Pagination    Feature = "pagination"
	Settings      Feature = "settings"
	Help          Feature = "help"
	Notifications Feature = "notifications"
	Export        Feature = "export"
	EmptyState    Feature = "empty_state"
	ErrorState    Feature = "error_state"
	LoadingState  Feature = "loading_state"
	Input         Feature = "text_input"
	Selection     Feature = "selection"
	Navigation    Feature = "navigation"
)

// Vocabulary lists every feature with the phrases that indicate it.
type phraseSet struct {
	Feature Feature
	Phrases []string
}

// featurePhrases is the deterministic lexicon. Phrases are matched case
// insensitively against screen text, advertised key hints and control labels.
var featurePhrases = []phraseSet{
	{Auth, []string{"sign in", "signin", "log in", "login", "authenticate", "password:", "enter password", "api key", "credentials"}},
	{PasswordReset, []string{"forgot password", "forgot your password", "reset password", "recover account", "password reset", "send reset"}},
	{Signup, []string{"sign up", "signup", "register", "create account", "new account"}},
	{TwoFactor, []string{"two-factor", "2fa", "verification code", "one-time code", "authenticator"}},
	{SessionEnd, []string{"sign out", "log out", "logout"}},
	{List, []string{}},
	{Detail, []string{}},
	{Create, []string{"new ", "add ", "create ", "compose", "duplicate"}},
	{Edit, []string{"edit ", "rename", "modify", "update "}},
	{Delete, []string{"delete", "remove", "archive", "trash", "destroy"}},
	{Confirm, []string{"are you sure", "confirm", "yes / no", "y/n", "cannot be undone"}},
	{Search, []string{"search", "find ", "filter by", "query"}},
	{Filter, []string{"filter", "sort", "group by"}},
	{Pagination, []string{"page ", "next page", "previous page", "more results", "load more"}},
	{Settings, []string{"settings", "preferences", "configure", "configuration", "options", "theme", "defaults"}},
	{Help, []string{"help", "documentation", "docs", "tutorial", "keybindings", "shortcuts"}},
	{Notifications, []string{"notifications", "inbox", "alerts", "messages", "unread"}},
	{Export, []string{"export", "download", "save as", "copy to clipboard"}},
	{EmptyState, []string{"(none)", "no items", "nothing here", "empty", "no results", "none yet"}},
	{ErrorState, []string{"error", "failed", "could not", "unable to", "try again", "retry"}},
	{LoadingState, []string{"loading", "please wait", "starting", "connecting", "scanning", "fetching"}},
	{Input, []string{}},
	{Selection, []string{}},
	{Navigation, []string{"back", "next", "previous", "up", "down", "tab", "focus", "select", "open"}},
}

// Lexicon returns the phrase table (used by the question builder and tests).
func Lexicon() []phraseSet { return featurePhrases }

// Strength says how much a hit is worth. A capability claimed from one word in
// prose is a hint; a capability that the interface advertises or that the screen
// structure shows is a fact about the software.
type Strength string

const (
	// StrengthProse: the phrase appeared in text somewhere on a screen.
	StrengthProse Strength = "prose"
	// StrengthAdvertised: a control label offers the capability.
	StrengthAdvertised Strength = "advertised"
	// StrengthStructural: the screen's structure demonstrates it (a form with a
	// password field, a list with rows, a detail view with aligned fields).
	StrengthStructural Strength = "structural"
)

// Evidence is where and how a feature was observed.
type Evidence struct {
	Feature  Feature       `json:"feature"`
	Screen   string        `json:"screen"`
	Grid     int           `json:"grid_id"`
	Region   screen.Region `json:"region"`
	Detail   string        `json:"detail"`
	Strength Strength      `json:"strength"`
	Observed int           `json:"observed"`
}

// strengthRank orders strengths so the strongest observation of a feature wins.
func strengthRank(strength Strength) int {
	switch strength {
	case StrengthStructural:
		return 3
	case StrengthAdvertised:
		return 2
	default:
		return 1
	}
}

// Inventory is everything the application was seen to offer during a run.
type Inventory struct {
	Hits        map[Feature][]Evidence `json:"hits"`
	Advertised  map[string][]Evidence  `json:"advertised"`
	Screens     []string               `json:"screens"`
	Transitions []Transition           `json:"transitions"`
	Lists       []ListShape            `json:"lists"`
	Structure   Structure              `json:"structure"`
}

// Transition is one observed move from a screen to another.
type Transition struct {
	From   string            `json:"from"`
	To     string            `json:"to"`
	Action string            `json:"action"`
	Detail map[string]string `json:"detail,omitempty"`
}

// ListShape describes a repeating row structure: how many rows, whether one is
// selected, and which per-row actions the screen advertises.
type ListShape struct {
	Screen   string   `json:"screen"`
	Rows     int      `json:"rows"`
	Selected bool     `json:"selected"`
	Actions  []string `json:"actions"`
	ItemKind string   `json:"item_kind,omitempty"`
}

// Structure holds the behavioural measurements that describe how the interface
// is put together rather than what it contains.
type Structure struct {
	MaxNesting       int               `json:"max_nesting"`
	NestingScreens   []string          `json:"nesting_screens"`
	MaxModalDepth    int               `json:"max_modal_depth"`
	FocusRegions     int               `json:"focus_regions"`
	DeadEndScreens   []string          `json:"dead_end_screens"`
	InertAffordances []InertAffordance `json:"inert_affordances"`
	RepeatedValues   []RepeatedValue   `json:"repeated_values"`
	ScreenCount      int               `json:"screen_count"`
}

// InertAffordance is a control the interface advertises that was tried and did
// nothing.
type InertAffordance struct {
	Label  string `json:"label"`
	Key    string `json:"key"`
	Screen string `json:"screen"`
	Tried  int    `json:"tried"`
}

// RepeatedValue is the same value shown in several places on one screen.
type RepeatedValue struct {
	Value  string `json:"value"`
	Count  int    `json:"count"`
	Screen string `json:"screen"`
}

// stateFeatures are the features whose own text *is* the evidence: when a screen
// says "are you sure?" that is a confirmation prompt, and when it says "(none)"
// that is an empty state. Capabilities such as search or export need a control
// or a structure to count, because the word alone proves nothing.
var stateFeatures = map[Feature]bool{
	Confirm:      true,
	EmptyState:   true,
	ErrorState:   true,
	LoadingState: true,
}

// NewInventory creates an empty inventory.
func NewInventory() *Inventory {
	return &Inventory{
		Hits:       map[Feature][]Evidence{},
		Advertised: map[string][]Evidence{},
		Screens:    []string{},
	}
}

// Has reports whether the application *has* a capability, which requires
// evidence stronger than a phrase appearing in prose. Words like "search" or
// "credentials" show up in ordinary text; a capability counts when the interface
// advertises it or the screen structure demonstrates it.
func (i *Inventory) Has(feature Feature) bool {
	for _, evidence := range i.Hits[feature] {
		if evidence.Strength != StrengthProse {
			return true
		}
	}
	return false
}

// Hinted reports whether a feature was only ever mentioned in prose.
func (i *Inventory) Hinted(feature Feature) bool {
	return len(i.Hits[feature]) > 0 && !i.Has(feature)
}

// MostConfident returns the strongest evidence for a feature.
func (i *Inventory) MostConfident(feature Feature) (Evidence, bool) {
	best, found := Evidence{}, false
	for _, evidence := range i.Hits[feature] {
		if !found || strengthRank(evidence.Strength) > strengthRank(best.Strength) {
			best, found = evidence, true
		}
	}
	return best, found
}

// Count returns how many times a feature was observed.
func (i *Inventory) Count(feature Feature) int { return len(i.Hits[feature]) }

// Record adds one observation.
func (i *Inventory) Record(evidence Evidence) {
	if evidence.Screen != "" && !containsString(i.Screens, evidence.Screen) {
		i.Screens = append(i.Screens, evidence.Screen)
	}
	existing := i.Hits[evidence.Feature]
	for index := range existing {
		if existing[index].Screen == evidence.Screen && existing[index].Detail == evidence.Detail {
			existing[index].Observed++
			// Keep the strongest claim about the same observation.
			if strengthRank(evidence.Strength) > strengthRank(existing[index].Strength) {
				existing[index].Strength = evidence.Strength
			}
			i.Hits[evidence.Feature] = existing
			return
		}
	}
	if evidence.Strength == "" {
		evidence.Strength = StrengthProse
	}
	evidence.Observed = 1
	i.Hits[evidence.Feature] = append(existing, evidence)
}

// RecordAffordance remembers a control the interface advertised but that the run
// never exercised.
func (i *Inventory) RecordAffordance(label, screen string, region screen.Region) {
	key := strings.ToLower(strings.TrimSpace(label))
	if key == "" {
		return
	}
	i.Advertised[key] = append(i.Advertised[key], Evidence{Screen: screen, Region: region, Detail: label})
}

// affordanceFeatures maps a control label to the capabilities it demonstrates.
// Words are matched on word boundaries so "open" does not fire on "opened by".
func affordanceFeatures(label string) []Feature {
	lower := " " + strings.ToLower(label) + " "
	out := []Feature{}
	for _, entry := range []struct {
		feature Feature
		words   []string
	}{
		{Detail, []string{"open", "view", "details", "inspect", "show"}},
		{Create, []string{"new", "add", "create", "compose"}},
		{Edit, []string{"edit", "rename", "modify", "update"}},
		{Delete, []string{"delete", "remove", "archive", "trash"}},
		{Search, []string{"search", "find", "filter"}},
		{Filter, []string{"filter", "sort", "group"}},
		{Pagination, []string{"next page", "prev page", "more", "load more"}},
		{Settings, []string{"settings", "preferences", "configure", "options"}},
		{Help, []string{"help", "docs", "shortcuts", "keybindings"}},
		{Export, []string{"export", "download", "save as", "copy"}},
		{PasswordReset, []string{"reset password", "forgot", "recover"}},
		{Signup, []string{"sign up", "register", "create account"}},
		{SessionEnd, []string{"sign out", "log out", "logout", "quit", "exit"}},
	} {
		for _, word := range entry.words {
			if strings.Contains(lower, " "+word+" ") || strings.HasSuffix(lower, " "+word+" ") {
				out = append(out, entry.feature)
				break
			}
		}
	}
	return out
}

// ObserveScreen derives feature hits from one captured screen. It is the whole
// deterministic half of the feature inventory: text, hints, structure and
// selection, with no model in the loop.
func (i *Inventory) ObserveScreen(grid *screen.Grid, screenName string, previous *screen.Grid, action string) {
	if grid == nil {
		return
	}
	structure := grid.Structure()
	lower := strings.ToLower(grid.TrimmedText())

	// Phrase hits, with the row they were found on. A phrase is only *evidence*
	// for a capability when the phrase is itself the thing (a confirmation
	// prompt, an empty state); otherwise it is a mention, and mentions are hints.
	for _, entry := range featurePhrases {
		strength := StrengthProse
		if stateFeatures[entry.Feature] {
			strength = StrengthStructural
		}
		for _, phrase := range entry.Phrases {
			index := strings.Index(lower, phrase)
			if index < 0 {
				continue
			}
			row := strings.Count(lower[:index], "\n")
			i.Record(Evidence{
				Feature:  entry.Feature,
				Screen:   screenName,
				Grid:     grid.ID,
				Region:   screen.Region{X0: 0, Y0: row, X1: grid.Cols - 1, Y1: row},
				Detail:   fmt.Sprintf("%q appears on this screen", phrase),
				Strength: strength,
			})
		}
	}

	// Advertised controls become affordances, and an advert for a capability is
	// itself evidence that the application offers it: a list that advertises
	// "Enter Open" has a way into an item even if the run never opened one. That
	// distinction is what keeps gap analysis honest in both directions.
	for _, hint := range structure.KeyHints {
		label := strings.TrimSpace(hint.Key + " " + hint.Label)
		i.RecordAffordance(label, screenName, screen.Region{X0: hint.X, Y0: hint.Y, X1: grid.Cols - 1, Y1: hint.Y})
		for _, match := range affordanceFeatures(label) {
			i.Record(Evidence{
				Feature:  match,
				Screen:   screenName,
				Grid:     grid.ID,
				Region:   screen.Region{X0: hint.X, Y0: hint.Y, X1: grid.Cols - 1, Y1: hint.Y},
				Detail:   fmt.Sprintf("advertised as %q", label),
				Strength: StrengthAdvertised,
			})
		}
	}

	// A list is several rows sharing a shape, or a row with a selection marker.
	if rows, selected := listShape(grid); rows >= 3 {
		actions := []string{}
		for _, hint := range structure.KeyHints {
			actions = append(actions, hint.Label)
		}
		i.Lists = append(i.Lists, ListShape{
			Screen:   screenName,
			Rows:     rows,
			Selected: selected,
			Actions:  actions,
			ItemKind: inferItemKind(grid),
		})
		i.Record(Evidence{
			Feature:  List,
			Screen:   screenName,
			Grid:     grid.ID,
			Region:   screen.Region{X0: 0, Y0: 0, X1: grid.Cols - 1, Y1: grid.RowsN - 1},
			Detail:   fmt.Sprintf("a list of about %d rows is shown", rows),
			Strength: StrengthStructural,
		})
	}

	// A detail view shows label/value pairs for one entity, usually with a
	// heading that matches an item previously seen in a list.
	if pairs, labels := detailShape(grid); pairs >= 3 {
		i.Record(Evidence{
			Feature:  Detail,
			Screen:   screenName,
			Grid:     grid.ID,
			Region:   screen.Region{X0: 0, Y0: 0, X1: grid.Cols - 1, Y1: grid.RowsN - 1},
			Detail:   fmt.Sprintf("a detail-style view with fields %s", strings.Join(labels, ", ")),
			Strength: StrengthStructural,
		})
	}

	// Text input: a bordered area with a cursor inside it.
	if field, ok := inputField(grid); ok {
		i.Record(Evidence{
			Feature:  Input,
			Screen:   screenName,
			Grid:     grid.ID,
			Region:   field,
			Detail:   "an editable text field is focused",
			Strength: StrengthStructural,
		})
	}

	// Selection: highlighted rows.
	if len(structure.Highlights) > 0 {
		i.Record(Evidence{
			Feature:  Selection,
			Screen:   screenName,
			Grid:     grid.ID,
			Region:   structure.Highlights[0].Region,
			Detail:   "a selection is highlighted",
			Strength: StrengthStructural,
		})
	}

	// A credential form is a capability the structure proves: a password field
	// with a way to submit it. Prose mentioning credentials is not enough.
	if evidence, ok := credentialForm(grid); ok {
		i.Record(evidence)
	}

	// Transitions: what this action moved the interface to.
	if previous != nil && previous.Digest != grid.Digest && action != "" {
		i.Transitions = append(i.Transitions, Transition{
			From:   screen.ScreenName(previous),
			To:     screenName,
			Action: action,
		})
	}

	i.Structure.ScreenCount = len(i.Screens)
}

// ObserveText derives feature hits from text rather than from a cell grid. The
// GUI path uses it on the vision tier's blinded description, so a GUI subject
// feeds the same inventory as a terminal subject.
func (i *Inventory) ObserveText(text, screenName string, gridID int) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if screenName != "" && !containsString(i.Screens, screenName) {
		i.Screens = append(i.Screens, screenName)
	}
	lower := strings.ToLower(text)
	for _, entry := range featurePhrases {
		for _, phrase := range entry.Phrases {
			if index := strings.Index(lower, phrase); index >= 0 {
				i.Record(Evidence{
					Feature: entry.Feature,
					Screen:  screenName,
					Grid:    gridID,
					Detail:  fmt.Sprintf("%q is described on this screen", phrase),
				})
			}
		}
	}
	// Advertised controls in a description read like "a button labelled Save".
	for _, match := range controlMentions(text) {
		i.RecordAffordance(match, screenName, screen.Region{})
		for _, feature := range affordanceFeatures(match) {
			i.Record(Evidence{
				Feature: feature,
				Screen:  screenName,
				Grid:    gridID,
				Detail:  fmt.Sprintf("described as %q", match),
			})
		}
	}
}

// controlMentions pulls short quoted labels out of a description.
func controlMentions(text string) []string {
	out := []string{}
	for _, quote := range []struct{ open, close string }{{"\"", "\""}, {"'", "'"}, {"“", "”"}} {
		remaining := text
		for {
			start := strings.Index(remaining, quote.open)
			if start < 0 {
				break
			}
			rest := remaining[start+len(quote.open):]
			end := strings.Index(rest, quote.close)
			if end < 0 {
				break
			}
			label := strings.TrimSpace(rest[:end])
			if len(label) >= 3 && len(label) <= 40 {
				out = append(out, label)
			}
			remaining = rest[end+len(quote.close):]
		}
	}
	return out
}

// credentialForm recognises a sign-in form from its structure.
func credentialForm(grid *screen.Grid) (Evidence, bool) {
	secretRow := -1
	for y := 0; y < grid.RowsN; y++ {
		text := strings.ToLower(grid.Row(y))
		if strings.Contains(text, "password") || strings.Contains(text, "passphrase") || strings.Contains(text, "••••") {
			secretRow = y
			break
		}
	}
	if secretRow < 0 {
		return Evidence{}, false
	}
	// It must also offer a way to submit, or it is documentation about a password.
	submit := false
	for _, hint := range grid.Structure().KeyHints {
		label := strings.ToLower(hint.Label + " " + hint.Key)
		if containsAny(label, []string{"submit", "sign in", "login", "log in", "continue", "enter"}) {
			submit = true
			break
		}
	}
	if !submit {
		text := strings.ToLower(grid.TrimmedText())
		submit = strings.Contains(text, "sign in") || strings.Contains(text, "log in") || strings.Contains(text, "login")
	}
	if !submit {
		return Evidence{}, false
	}
	return Evidence{
		Feature:  Auth,
		Screen:   screen.ScreenName(grid),
		Grid:     grid.ID,
		Region:   screen.Region{X0: 0, Y0: secretRow, X1: grid.Cols - 1, Y1: secretRow},
		Detail:   "the screen contains a credential field with a way to submit it",
		Strength: StrengthStructural,
	}, true
}

// FinishStructure computes the behavioural measurements that need the whole run.
func (i *Inventory) FinishStructure(deadEnds []string, inert []InertAffordance) {
	i.Structure.DeadEndScreens = deadEnds
	i.Structure.InertAffordances = inert
	seen := map[string]bool{}
	for _, shape := range i.Lists {
		if shape.Selected {
			seen[shape.Screen] = true
		}
	}
	i.Structure.MaxNesting = 0
	i.Structure.NestingScreens = nil
}

// FeatureList returns every observed feature, sorted, with its hit count.
func (i *Inventory) FeatureList() []Feature {
	out := make([]Feature, 0, len(i.Hits))
	for feature := range i.Hits {
		out = append(out, feature)
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

// Summary renders the inventory for a model prompt: what exists, what was
// advertised, and how the interface is structured. Grounding the completeness
// questions in this text is what stops the model inventing features.
func (i *Inventory) Summary() string {
	var builder strings.Builder
	builder.WriteString("FEATURES OBSERVED (measured, not guessed):\n")
	if len(i.Hits) == 0 {
		builder.WriteString("- none\n")
	}
	for _, feature := range i.FeatureList() {
		evidence := i.Hits[feature]
		screens := map[string]bool{}
		for _, entry := range evidence {
			screens[entry.Screen] = true
		}
		names := make([]string, 0, len(screens))
		for name := range screens {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintf(&builder, "- %s (%d observation(s) on %s)\n", feature, len(evidence), strings.Join(names, ", "))
	}
	if len(i.Screens) > 0 {
		fmt.Fprintf(&builder, "SCREENS VISITED: %s\n", strings.Join(i.Screens, " | "))
	}
	if len(i.Lists) > 0 {
		builder.WriteString("LISTS SEEN:\n")
		for _, shape := range i.Lists {
			fmt.Fprintf(&builder, "- on %s: %d rows, selection=%t, actions advertised: %s\n",
				shape.Screen, shape.Rows, shape.Selected, strings.Join(shape.Actions, " / "))
		}
	}
	if len(i.Advertised) > 0 {
		labels := make([]string, 0, len(i.Advertised))
		for label := range i.Advertised {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		if len(labels) > 20 {
			labels = labels[:20]
		}
		fmt.Fprintf(&builder, "CONTROLS ADVERTISED: %s\n", strings.Join(labels, " | "))
	}
	if len(i.Transitions) > 0 {
		builder.WriteString("MOVES OBSERVED:\n")
		limit := len(i.Transitions)
		if limit > 12 {
			limit = 12
		}
		for _, transition := range i.Transitions[:limit] {
			fmt.Fprintf(&builder, "- %s -> %s (via %s)\n", transition.From, transition.To, transition.Action)
		}
	}
	return builder.String()
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
