package audit

import (
	"strings"
	"testing"

	"github.com/carlvincetan/argus/internal/screen"
)

const loginScreen = `┌─ Sign in ────────────────────────────────┐
│                                          │
│  Email:    user@example.com              │
│  Password: ••••••••                      │
│                                          │
│            [ Sign in ]                   │
│                                          │
└──────────────────────────────────────────┘
Enter Submit   Esc Quit`

const loginWithReset = `┌─ Sign in ────────────────────────────────┐
│                                          │
│  Email:    user@example.com              │
│  Password: ••••••••                      │
│                                          │
│            [ Sign in ]                   │
│  Forgot password? Press R                │
└──────────────────────────────────────────┘
Enter Submit   R Reset password   Esc Quit`

const connectionsList = `┌─ Connections ────────────────────────────┐
│ > prod-eu-1      connected    12 ms      │
│   prod-us-2      connected    38 ms      │
│   staging-eu     idle         91 ms      │
│   dev-local      offline       --        │
└──────────────────────────────────────────┘
Esc Quit   / Filter`

const connectionsWithDetail = `┌─ Connections ────────────────────────────┐
│ > prod-eu-1      connected    12 ms      │
│   prod-us-2      connected    38 ms      │
│   staging-eu     idle         91 ms      │
└──────────────────────────────────────────┘
Enter Open   E Edit   Esc Quit`

const nestedPanels = `┌─ Agents ─────────────────────────────────┐
│ ┌─ Agent: coder ───────────────────────┐ │
│ │ ┌─ Model: gpt-x ───────────────────┐ │ │
│ │ │ ┌─ Thinking ───────────────────┐ │ │ │
│ │ │ │ planning the change…         │ │ │ │
│ │ │ └──────────────────────────────┘ │ │ │
│ │ └──────────────────────────────────┘ │ │
│ └──────────────────────────────────────┘ │
└──────────────────────────────────────────┘`

// inventoryFrom runs the deterministic half over one synthetic screen, which is
// exactly what the session does for every captured frame.
func inventoryFrom(screenText, name string) *Inventory {
	inv := NewInventory()
	grid := screen.NewGrid(strings.Split(screenText, "\n"))
	inv.ObserveScreen(grid, name, nil, "startup")
	inv.ObserveStructure(grid, name)
	return inv
}

func hasGap(inv *Inventory, missing Feature) bool {
	for _, gap := range Gaps(inv) {
		if gap.Missing == missing {
			return true
		}
	}
	return false
}

func hasCandidate(inv *Inventory, id string) bool {
	for _, candidate := range Candidates(inv) {
		if candidate.ID == id {
			return true
		}
	}
	return false
}

// hasCandidatePrefix matches ids that carry a screen name suffix.
func hasCandidatePrefix(inv *Inventory, prefix string) bool {
	for _, candidate := range Candidates(inv) {
		if strings.HasPrefix(candidate.ID, prefix) {
			return true
		}
	}
	return false
}

func TestAuthWithoutResetIsAGap(t *testing.T) {
	inv := inventoryFrom(loginScreen, "Sign in")
	if !inv.Has(Auth) {
		t.Fatalf("auth was not recognised on a sign-in screen: %v", inv.FeatureList())
	}
	if inv.Has(PasswordReset) {
		t.Fatal("a locked-out hint should not be recorded on a screen without one")
	}
	if !hasGap(inv, PasswordReset) {
		t.Fatalf("expected a password-reset gap, got %+v", Gaps(inv))
	}
	// The gap must be evidence-backed, not free-floating.
	gap := Gaps(inv)[0]
	if len(gap.Evidence) == 0 || gap.Evidence[0].Screen != "Sign in" {
		t.Fatalf("gap has no evidence: %+v", gap)
	}
}

func TestAuthWithResetHasNoGap(t *testing.T) {
	inv := inventoryFrom(loginWithReset, "Sign in")
	if !inv.Has(PasswordReset) {
		t.Fatalf("reset affordance was not recognised: %v", inv.FeatureList())
	}
	if hasGap(inv, PasswordReset) {
		t.Fatalf("no gap should be reported when the reset path is advertised: %+v", Gaps(inv))
	}
}

func TestConnectionsListWithoutDetailIsAGap(t *testing.T) {
	inv := inventoryFrom(connectionsList, "Connections")
	if !inv.Has(List) {
		t.Fatalf("a list of connections was not recognised as a list: %v", inv.FeatureList())
	}
	if !hasGap(inv, Detail) {
		t.Fatalf("expected a detail-view gap for an unopenable list, got %+v", Gaps(inv))
	}
	if !hasCandidate(inv, "gap:list-without-detail_view") {
		t.Fatalf("expected the gap as a review candidate: %+v", Candidates(inv))
	}
}

func TestListWithOpenActionHasNoDetailGap(t *testing.T) {
	inv := inventoryFrom(connectionsWithDetail, "Connections")
	if hasGap(inv, Detail) {
		t.Fatalf("list rows that can be opened should not be a gap: %+v", Gaps(inv))
	}
}

func TestNestedPanelsAreStructuralCandidate(t *testing.T) {
	inv := inventoryFrom(nestedPanels, "Agents")
	if inv.Structure.MaxNesting < 3 {
		t.Fatalf("expected three levels of nesting, measured %d", inv.Structure.MaxNesting)
	}
	if !hasCandidate(inv, "structure:nesting") {
		t.Fatalf("expected a nesting candidate: %+v", Candidates(inv))
	}
}

func TestDeadEndScreenIsBehaviourCandidate(t *testing.T) {
	inv := inventoryFrom(`┌─ Status ────────────────┐
│ all systems nominal     │
│ nothing to do here      │
└─────────────────────────┘
Esc Close   Q Quit`, "Status")
	if len(inv.Structure.DeadEndScreens) == 0 {
		t.Fatalf("a screen whose only controls leave it should be a dead end: %+v", inv.Structure)
	}
	if !hasCandidatePrefix(inv, "behaviour:dead-end") {
		t.Fatalf("expected a dead-end candidate: %+v", Candidates(inv))
	}
}

func TestInertAffordanceIsBehaviourCandidate(t *testing.T) {
	inv := inventoryFrom(connectionsList, "Connections")
	inv.RecordInertAffordance("R Retry", "Connections", 2)
	if !hasCandidate(inv, "behaviour:inert-affordance") {
		t.Fatalf("expected an inert-affordance candidate: %+v", Candidates(inv))
	}
}

func TestInventorySummaryIsGroundingText(t *testing.T) {
	inv := inventoryFrom(connectionsList, "Connections")
	summary := inv.Summary()
	for _, needle := range []string{"FEATURES OBSERVED", "list", "LISTS SEEN", "Connections"} {
		if !strings.Contains(summary, needle) {
			t.Fatalf("summary is missing %q:\n%s", needle, summary)
		}
	}
}

func TestPaginationOnlyExpectedOnLongLists(t *testing.T) {
	short := inventoryFrom(connectionsList, "Connections")
	if hasGap(short, Pagination) {
		t.Fatalf("a four-row list should not require pagination: %+v", Gaps(short))
	}
	lines := []string{"┌─ Connections ────────────────────────────┐"}
	for index := 0; index < 30; index++ {
		lines = append(lines, "│ > connection-"+string(rune('a'+index%26))+string(rune('0'+index/26))+"   connected   │")
	}
	lines = append(lines, "└──────────────────────────────────────────┘")
	long := inventoryFrom(strings.Join(lines, "\n"), "Connections")
	if !hasGap(long, Pagination) && !hasGap(long, Search) {
		t.Fatalf("a long list should expect search or pagination: %+v", Gaps(long))
	}
}

func TestLoginScreenIsNotAListOrDetailView(t *testing.T) {
	// The sign-in screen has an "Email: value" row, a masked password row and a
	// hint bar. None of that makes it a list or a detail view, and treating it
	// as one hides the gaps the auditor exists to find.
	inv := inventoryFrom(loginScreen, "Sign in")
	if inv.Has(List) {
		t.Fatalf("a login form was read as a list: %v", inv.FeatureList())
	}
	if inv.Has(Detail) {
		t.Fatalf("a login form was read as a detail view: %v", inv.FeatureList())
	}
	if !hasGap(inv, PasswordReset) {
		t.Fatalf("expected the missing recovery path to be reported: %+v", Gaps(inv))
	}
}

func TestHintBarIsNotContent(t *testing.T) {
	inv := inventoryFrom(`┌─ Queue ──────────────────────┐
│  build-1   running          │
│  build-2   queued           │
│  build-3   failed           │
└─────────────────────────────┘
Esc Quit   Enter Open   D Delete`, "Queue")
	if !inv.Has(List) {
		t.Fatalf("three consecutive rows are a list: %v", inv.FeatureList())
	}
	// The hint bar advertises Open and Delete, so both capabilities exist.
	if !inv.Has(Detail) || !inv.Has(Delete) {
		t.Fatalf("advertised controls should count as capabilities: %v", inv.FeatureList())
	}
	if hasGap(inv, Detail) {
		t.Fatalf("an advertised open action means the detail path exists: %+v", Gaps(inv))
	}
	// Nothing on the screen asks the user to confirm the delete, so this is the
	// gap the expectation table exists to catch.
	if !hasGap(inv, Confirm) {
		t.Fatalf("deleting with no confirmation is a gap: %+v", Gaps(inv))
	}
}

func TestAdvertisedConfirmationClosesTheDeleteGap(t *testing.T) {
	inv := inventoryFrom(`┌─ Queue ──────────────────────┐
│  build-1   running          │
│  build-2   queued           │
│  build-3   failed           │
└─────────────────────────────┘
Are you sure?  Y Confirm   N Cancel`, "Queue")
	if !inv.Has(Confirm) {
		t.Fatalf("a confirmation prompt was not recognised: %v", inv.FeatureList())
	}
	if hasGap(inv, Confirm) {
		t.Fatalf("with a confirmation prompt present the gap is closed: %+v", Gaps(inv))
	}
}

func TestFieldRowsAreNotListItems(t *testing.T) {
	// A record of one entity ("model: gpt-x") is not a list of three items.
	inv := inventoryFrom(`┌─ Agent: coder ───────────────────────┐
│ model:    gpt-x                       │
│ thinking: planning the change         │
│ status:   running                     │
└───────────────────────────────────────┘
 Esc Back   Q Quit`, "Agent: coder")
	if inv.Has(List) {
		t.Fatalf("label/value rows were read as a list: %v", inv.FeatureList())
	}
	if !inv.Has(Detail) {
		t.Fatalf("three aligned fields are a detail view: %v", inv.FeatureList())
	}
}

func TestProseMentionIsNotACapability(t *testing.T) {
	// A word in ordinary text must not make the auditor believe a feature
	// exists. "credentials" here is prose, not a sign-in form.
	inv := inventoryFrom(`┌─ Settings ────────────────────────────┐
│ Store your credentials in the vault.  │
│ Search is available from the palette. │
└───────────────────────────────────────┘
 Esc Back`, "Settings")
	if inv.Has(Auth) {
		t.Fatalf("prose about credentials is not a sign-in capability: %v", inv.FeatureList())
	}
	if inv.Has(Search) {
		t.Fatalf("prose about search is not a search capability: %v", inv.FeatureList())
	}
	if !inv.Hinted(Auth) {
		t.Fatal("the prose hit should still be recorded as a hint")
	}
}

func TestSignInFormIsDetectedFromStructure(t *testing.T) {
	inv := inventoryFrom(loginScreen, "Sign in")
	if !inv.Has(Auth) {
		t.Fatalf("a password field with a submit affordance is a sign-in form: %v", inv.FeatureList())
	}
	evidence, ok := inv.MostConfident(Auth)
	if !ok || evidence.Strength != StrengthStructural {
		t.Fatalf("expected structural evidence for the sign-in form: %+v", evidence)
	}
}

func TestAdvertisedControlBeatsProse(t *testing.T) {
	inv := inventoryFrom(`┌─ Queue ──────────────────────┐
│  build-1   running          │
│  build-2   queued           │
│  build-3   failed           │
└─────────────────────────────┘
N New   Esc Quit`, "Queue")
	if !inv.Has(Create) {
		t.Fatalf("an advertised New control is a create capability: %v", inv.FeatureList())
	}
}
