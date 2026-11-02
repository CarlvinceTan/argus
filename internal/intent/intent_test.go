package intent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ledgerRepo(t *testing.T) (string, *Ledger) {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "e2e"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "View.swift", "tapArea(14)")
	ledger, err := Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	return repo, ledger
}

func write(t *testing.T, repo, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sample() Intent {
	return Intent{ID: "tap", Check: "small", Element: `Button "Dismiss*"`, Reason: "hit area is wider", Paths: []string{"View.swift"}}
}

func TestChangedSourceMakesAnIntentStaleUntilConfirmed(t *testing.T) {
	repo, ledger := ledgerRepo(t)
	if err := ledger.Add(sample()); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Save(); err != nil {
		t.Fatal(err)
	}
	fresh, err := Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Stale("tap") {
		t.Fatal("an untouched intent went stale")
	}
	write(t, repo, "View.swift", "tapArea(4)")
	changed, err := Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !changed.Stale("tap") {
		t.Fatal("a changed source left the intent fresh")
	}
	if strings.Contains(changed.Policy(), "hit area") {
		t.Fatal("a model was told a stale intent is settled")
	}
	if err := changed.Confirm("tap"); err != nil {
		t.Fatal(err)
	}
	if err := changed.Save(); err != nil {
		t.Fatal(err)
	}
	confirmed, err := Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Stale("tap") || !strings.Contains(confirmed.Policy(), "hit area") {
		t.Fatal("a confirmed intent should be fresh and known to a model")
	}
}

func TestAMissingSourceMakesAnIntentStale(t *testing.T) {
	repo, ledger := ledgerRepo(t)
	if err := ledger.Add(sample()); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "View.swift")); err != nil {
		t.Fatal(err)
	}
	gone, err := Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !gone.Stale("tap") {
		t.Fatal("an intent resting on a deleted file stayed fresh")
	}
	if err := gone.Confirm("tap"); err == nil {
		t.Fatal("an intent resting on a deleted file was confirmed")
	}
}

func TestAddRefusesWhatCouldNeverGoStale(t *testing.T) {
	_, ledger := ledgerRepo(t)
	entry := sample()
	entry.Paths = nil
	if err := ledger.Add(entry); err == nil {
		t.Fatal("an intent with no sources was accepted")
	}
	entry = sample()
	entry.Reason = ""
	if err := ledger.Add(entry); err == nil {
		t.Fatal("an intent with no reason was accepted")
	}
	if err := ledger.Add(sample()); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Add(sample()); err == nil {
		t.Fatal("a duplicate id was accepted")
	}
}

func TestMatchIsScopedToCheckElementAndJourney(t *testing.T) {
	_, ledger := ledgerRepo(t)
	entry := sample()
	entry.Journey = "chat"
	if err := ledger.Add(entry); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		check, element, journey string
		want                    bool
	}{
		{"small", `Button "Dismiss warning"`, "chat", true},
		{"overlap", `Button "Dismiss warning"`, "chat", false},
		{"small", `Button "Send"`, "chat", false},
		{"small", `Button "Dismiss warning"`, "profile", false},
	}
	for _, test := range cases {
		if _, got := ledger.Match(test.check, test.element, test.journey); got != test.want {
			t.Fatalf("Match(%q, %q, %q) = %t", test.check, test.element, test.journey, got)
		}
	}
	wide := sample()
	wide.ID, wide.Check, wide.Element = "avatar", "drift_*", "* avatar, Idle"
	if err := ledger.Add(wide); err != nil {
		t.Fatal(err)
	}
	if _, ok := ledger.Match("drift_added", "Image avatar, Idle", "any"); !ok {
		t.Fatal("a check pattern didn't match")
	}
	var none *Ledger
	if _, ok := none.Match("small", "x", "y"); ok {
		t.Fatal("an absent ledger matched")
	}
}
