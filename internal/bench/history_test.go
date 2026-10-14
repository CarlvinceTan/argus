package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoryKeepsAcceptedHostTimingAndGlobalQuality(t *testing.T) {
	name := filepath.Join(t.TempDir(), "history.tsv")
	rows := []Summary{
		{Host: "fast", WallSeconds: 10, Caught: 5},
		{Host: "slow", WallSeconds: 100, Caught: 5},
		// Unexplained failures on either host never advance the baseline.
		{Host: "fast", WallSeconds: 12, Caught: 5},
		{Host: "slow", WallSeconds: 100, Caught: 4, FalsePositives: 1},
	}
	for _, row := range rows {
		if err := appendHistory(name, row); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"fast", "slow", "new"} {
		baseline, err := lastHistory(name, host)
		if err != nil || baseline == nil || baseline.Caught != 5 || baseline.FalsePositives != 0 {
			t.Fatalf("%s baseline: %+v %v", host, baseline, err)
		}
		wall := map[string]float64{"fast": 10, "slow": 100, "new": 0}[host]
		if baseline.WallSeconds != wall {
			t.Fatalf("%s timing: %+v", host, baseline)
		}
	}
	accepted := Summary{Host: "fast", WallSeconds: 12, Caught: 4, FalsePositives: 1, Reason: "PR explains additional checks and corpus change."}
	if err := appendHistory(name, accepted); err != nil {
		t.Fatal(err)
	}
	baseline, err := lastHistory(name, "fast")
	if err != nil || baseline.Caught != 4 || baseline.FalsePositives != 1 || baseline.WallSeconds != 12 {
		t.Fatalf("accepted baseline: %+v %v", baseline, err)
	}
	slow, err := lastHistory(name, "slow")
	if err != nil || slow.Caught != 4 || slow.WallSeconds != 100 {
		t.Fatalf("other host baseline: %+v %v", slow, err)
	}
	after, err := os.ReadFile(name)
	if err != nil || !strings.HasPrefix(string(after), string(before)) {
		t.Fatal("history rewritten")
	}
}
