package harness

import (
	"testing"
	"time"
)

func TestRemoteX11RejectsDeadAgent(t *testing.T) {
	start := time.Now()
	if _, err := StartRemoteX11("127.0.0.1:1", ""); err == nil {
		t.Fatal("expected a connection error for a closed port")
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("connecting to a dead agent took %s", elapsed)
	}
}

func TestRemoteX11ReconnectFailsCleanly(t *testing.T) {
	remote := &RemoteX11{address: "127.0.0.1:1", started: time.Now()}
	if err := remote.reconnect(); err == nil {
		t.Fatal("expected reconnect to a dead agent to fail")
	}
}
