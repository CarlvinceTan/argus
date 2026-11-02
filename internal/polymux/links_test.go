package polymux

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlanJourneysAddsJourneysABaselineLinksToTheChange(t *testing.T) {
	repo := journeyRepo(t)
	write := func(name, text string) {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The area map sends ChatPane.swift to the chat journeys only, but the
	// profile journey's approved screens show a label ChatPane draws.
	write("apps/Polymux/Sources/Views/ChatPane.swift", `Button("Rename bot") {}
Button("Done") {}`)
	write("e2e/baselines/fixture-profile.json", `{"state": {"rows": ["AXButton|Rename bot|0,0,1,1|", "AXButton|Done|0,0,1,1|"]}}`)
	journeys, err := PlanJourneys(repo, []string{"apps/Polymux/Sources/Views/ChatPane.swift"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	linked := map[string]string{}
	for _, journey := range journeys {
		linked[journey.ID] = journey.Linked
	}
	if len(journeys) != 3 || linked["fixture-profile"] != `"Rename bot" in ChatPane.swift` {
		t.Fatalf("journeys %v", linked)
	}
	if linked["fixture-chat-place"] != "" {
		t.Fatalf("a rule-chosen journey carries a link: %v", linked)
	}

	// One word is too common to mean a dependency.
	write("apps/Polymux/Sources/Views/ChatPane.swift", `Button("Done") {}`)
	journeys, err = PlanJourneys(repo, []string{"apps/Polymux/Sources/Views/ChatPane.swift"}, 0)
	if err != nil || len(journeys) != 2 {
		t.Fatalf("journeys %v, %v", journeys, err)
	}
}

func TestPlanJourneysSkipsServerCodeAFixtureRunNeverBuilds(t *testing.T) {
	repo := journeyRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	runScript := "if [ \"$fixture\" = 0 ]; then build_server; fi\n" +
		`fingerprint "$ROOT/apps/server" "$ROOT/packages/runtime" "$ROOT/go.work" >"$STATE/server.stamp"` + "\n"
	if err := os.WriteFile(filepath.Join(repo, "scripts/run.sh"), []byte(runScript), 0o644); err != nil {
		t.Fatal(err)
	}
	server := []string{"apps/server/internal/chat/reply.go", "packages/runtime/loop.go", "go.work"}
	journeys, err := PlanJourneys(repo, server, 0)
	if err != nil || len(journeys) != 0 {
		t.Fatalf("server-only change planned %v, %v", journeys, err)
	}
	// Mixed with app code, the app code still decides; packages/runtimeish isn't runtime.
	journeys, err = PlanJourneys(repo, append(server, "apps/Polymux/Sources/Views/ProfileView.swift"), 0)
	if err != nil || len(journeys) != 1 || journeys[0].ID != "fixture-profile" {
		t.Fatalf("mixed change planned %v, %v", journeys, err)
	}
	if got := withoutServer(repo, []string{"packages/runtimeish/x.go"}); len(got) != 1 {
		t.Fatalf("a prefix that isn't the directory was dropped: %v", got)
	}
}
