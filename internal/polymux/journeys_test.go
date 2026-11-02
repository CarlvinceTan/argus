package polymux

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func journeyRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	write := func(name, text string) {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("e2e/areas.json", `{
		"ignore": ["docs/**", "*.md"],
		"always": ["smoke.js"],
		"areas": [{"paths": ["*/Views/ChatPane.swift"], "flows": ["fixture-chat*", "chat.js"]},
		          {"paths": ["*/Views/ProfileView.swift"], "flows": ["fixture-profile.js"]}]
	}`)
	write("e2e/flows/chat.js", "const real = true;\n")
	write("e2e/flows/smoke.js", "const real = true;\n")
	write("e2e/flows/fixture-chat.js", "// @fixture --verify-fixture --verify-tools\n")
	write("e2e/flows/fixture-chat-place.js", "// @fixture --verify-fixture --verify-tools\n")
	write("e2e/flows/fixture-chat-long.js", "// @fixture --verify-fixture --verify-history --verify-still\n")
	write("e2e/flows/fixture-profile.js", "// @fixture --verify-fixture --verify-profile\n")
	write("e2e/flows/fixture-approval.js", "// @fixture --verify-approval\n")
	return repo
}

func TestPlanJourneys(t *testing.T) {
	repo := journeyRepo(t)
	journeys, err := PlanJourneys(repo, []string{"apps/Polymux/Sources/Views/ChatPane.swift"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, journey := range journeys {
		got = append(got, journey.ID+" "+strings.Join(journey.Flags, " "))
	}
	// Shared states collapse, most-covered first; real-server flows have no fixture state.
	want := []string{
		"fixture-chat-place --verify-tools --verify-still",
		"fixture-chat-long --verify-history --verify-still",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("journeys = %q, want %q", got, want)
	}
	if len(journeys[0].Flows) != 2 {
		t.Fatalf("flows = %v", journeys[0].Flows)
	}
}

func TestPlanJourneysSelection(t *testing.T) {
	repo := journeyRepo(t)
	cases := []struct {
		name    string
		changed []string
		want    int
	}{
		{"nothing changed plans the whole surface", nil, 3},
		{"an ignored file plans nothing", []string{"docs/control.md"}, 0},
		{"an unmapped app source could affect anything", []string{"apps/Polymux/Sources/New.swift"}, 3},
		{"a changed flow plans itself", []string{"e2e/flows/fixture-profile.js"}, 1},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			journeys, err := PlanJourneys(repo, test.changed, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(journeys) != test.want {
				t.Fatalf("got %d journeys: %+v", len(journeys), journeys)
			}
		})
	}
}

func TestShardsCoverEveryJourneyOnce(t *testing.T) {
	journeys := []Journey{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}, {ID: "e"}}
	seen := map[string]int{}
	for index := 1; index <= 2; index++ {
		for _, journey := range Shard(journeys, index, 2) {
			seen[journey.ID]++
		}
	}
	if len(seen) != len(journeys) {
		t.Fatalf("shards covered %v", seen)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("%s ran %d times", id, count)
		}
	}
}

func TestScanLogReportsEachWarningOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	text := "started\nPublishing changes from within view updates is not allowed\nPublishing changes from within view updates is not allowed\nready\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	found := ScanLog(path)
	if len(found) != 1 || !strings.Contains(found[0].Detail, "Publishing changes") {
		t.Fatalf("found = %+v", found)
	}
	if got := ScanLog(filepath.Join(t.TempDir(), "missing")); got != nil {
		t.Fatalf("a missing log gave %+v", got)
	}
}
