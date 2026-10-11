package provider

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/jev"
	"github.com/carlvincetan/argus/internal/knowledge"
)

func TestLiveSystemOneQuestionIsolation(t *testing.T) {
	if testing.Short() || os.Getenv("ARGUS_LIVE_SYSTEMONE") != "1" {
		t.Skip("live test")
	}
	cfg := config.Default()
	cfg.Provider = "systemone"
	cfg.ApplyProviderDefaults()
	client := jev.NewClient(cfg.JevEndpoint, cfg.JevModel, "", 40*time.Second)
	questions := map[string]jev.Question{}
	for id, instruction := range knowledge.ScreenQuestions() {
		switch id {
		case "defect_class", "unstated_what":
			questions[id] = jev.Choice(instruction, knowledge.ClassCriteria())
		case "severity", "unstated_severity":
			questions[id] = jev.Score(instruction, knowledge.SeverityScoreLevels())
		case "defect_row":
			questions[id] = jev.Choice(instruction, map[string]string{"none": "no row", "1": "header row"})
		default:
			questions[id] = jev.Noul(instruction)
		}
	}
	questions, _ = systemOneQuestions(questions)
	state := map[string]any{"screen": map[string]any{"text": "Sign in form, email field, password field, sign in button."}, "policy": knowledge.Policy}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	for id, question := range questions {
		_, err := client.Decide(ctx, state, map[string]jev.Question{id: question}, 1, nil)
		if err != nil {
			t.Logf("BAD %s type=%s: %v", id, question.Type, err)
		} else {
			t.Logf("OK  %s type=%s", id, question.Type)
		}
	}
}

func TestLiveSystemOneQuestionBatches(t *testing.T) {
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
	ids := make([]string, 0, len(questions))
	for id := range questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for start := 0; start < len(ids); start += 4 {
		end := min(start+4, len(ids))
		batch := map[string]jev.Question{}
		for _, id := range ids[start:end] {
			batch[id] = questions[id]
		}
		_, err := providers.Decisions.Decide(ctx, map[string]any{
			"screen": map[string]any{"text": "Sign in form with email and password fields, submit button."},
			"policy": knowledge.Policy,
		}, batch, 1, nil)
		if err != nil {
			t.Logf("BAD batch %v: %v", ids[start:end], err)
		} else {
			t.Logf("OK batch %v", ids[start:end])
		}
	}
}

func TestLiveSystemOneSequentialBatches(t *testing.T) {
	if os.Getenv("ARGUS_LIVE_SYSTEMONE") != "1" {
		t.Skip("set ARGUS_LIVE_SYSTEMONE=1")
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
	for index := 0; index < 3; index++ {
		_, err := providers.Decisions.Decide(context.Background(), map[string]any{
			"app":    map[string]any{"name": "auth-no-reset", "screen": "Sign in"},
			"screen": map[string]any{"text": "Blinded description: sign in form with email and password fields, submit button."},
			"policy": knowledge.Policy,
		}, questions, 1, nil)
		if err != nil {
			t.Fatalf("sequential call %d: %v", index, err)
		}
	}
}
