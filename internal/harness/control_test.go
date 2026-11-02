package harness

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeControl speaks the helper's protocol. Tokens carry the snapshot number, so
// a press with an old token is refused the way the real helper refuses it.
const fakeControl = `#!/bin/sh
case "$1" in
permissions)
  echo '{"ok":true,"summary":"s","accessibility":'"$GRANTED"',"screenRecording":true,"executable":"'"$0"'"}' ;;
session)
  echo '{"ok":true,"summary":"ready","target":{"pid":'$$'}}'
  n=0
  while read -r line; do
    case "$line" in
    *'"snapshot"'*)
      n=$((n + 1))
      echo '{"ok":true,"complete":true,"elements":[{"index":0,"role":"AXWindow","label":"App","frame":{"x":0,"y":0,"w":800,"h":600}},{"index":1,"parent":0,"role":"AXButton","label":"Send","token":"t'$n'","actions":["AXPress"],"frame":{"x":10,"y":10,"w":80,"h":30}}],"windows":[{"window":"w-1","captureWindow":7,"main":true,"frame":{"x":0,"y":0,"w":800,"h":600}}]}' ;;
    *'"close"'*)
      echo '{"ok":true}'; exit 0 ;;
    *"\"t$n\""*)
      echo '{"ok":true,"effect":"observe"}' ;;
    *)
      echo '{"ok":false,"code":"stale_token","summary":"stale"}' ;;
    esac
  done ;;
esac
`

func controlRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scripts/control.sh"), []byte(fakeControl), 0o755); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestControlResolvesKeysOnAFreshSnapshot(t *testing.T) {
	t.Setenv("GRANTED", "true")
	h, err := StartControl(ControlOptions{Repo: controlRepo(t), Instance: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	snapshot, err := h.ObserveAX()
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Complete || snapshot.Surface != "w-1" || snapshot.Window.W != 800 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if err := h.Press("AXButton|Send#0"); err != nil {
		t.Fatalf("a key from an old snapshot should still press: %v", err)
	}
	if err := h.Press("AXButton|Gone#0"); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("a missing element returned %v", err)
	}
	if !h.Alive() {
		t.Fatal("the helper process should count as alive")
	}
	if len(h.Inputs()) != 2 {
		t.Fatalf("inputs = %+v", h.Inputs())
	}
}

func TestControlRefusesWithoutPermission(t *testing.T) {
	t.Setenv("GRANTED", "false")
	_, err := StartControl(ControlOptions{Repo: controlRepo(t), Instance: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "Accessibility") {
		t.Fatalf("err = %v", err)
	}
}

func TestControlBusyOnlyRetriesAppStates(t *testing.T) {
	if !controlBusy(&ControlError{Code: "ax_unavailable"}) {
		t.Fatal("an unreadable tree is worth retrying")
	}
	if controlBusy(&ControlError{Code: "stale_token"}) || controlBusy(errors.New("session ended")) {
		t.Fatal("only a busy app is worth retrying")
	}
}
