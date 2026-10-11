package provider

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/jev"
	"github.com/carlvincetan/argus/internal/knowledge"
)

func TestLiveSystemOneFullScreenQuestionBank(t *testing.T) {
	if os.Getenv("ARGUS_LIVE_SYSTEMONE") != "1" {
		t.Skip("set ARGUS_LIVE_SYSTEMONE=1 for live endpoint validation")
	}
	cfg := config.Default()
	cfg.Provider = "systemone"
	cfg.ApplyProviderDefaults()
	providers, err := Build(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	questions := map[string]jev.Question{}
	for id, instruction := range knowledge.ScreenQuestions() {
		switch id {
		case "defect_class", "unstated_what":
			questions[id] = jev.Choice(instruction, knowledge.ClassCriteria())
		case "severity", "unstated_severity":
			questions[id] = jev.Score(instruction, knowledge.SeverityScoreLevels())
		default:
			questions[id] = jev.Noul(instruction)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := providers.Decisions.Decide(ctx, map[string]any{
		"screen": map[string]any{"text": "Sign in form with email and password fields, submit button."},
		"policy": knowledge.Policy,
	}, questions, 1, map[string]any{"purpose": "live-regression"})
	if err != nil {
		t.Fatalf("System One full question bank: %v", err)
	}
	if _, ok := result.Answers["guidance"]; !ok {
		t.Fatal("guidance answer missing")
	}
	t.Logf("answers=%d model=%s guidance=%.4f", len(result.Answers), result.Model, result.Answers["guidance"].P())
}
