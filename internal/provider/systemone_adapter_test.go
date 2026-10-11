package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/carlvincetan/argus/internal/jev"
	"github.com/carlvincetan/argus/internal/knowledge"
)

func TestSystemOneNormalizesAllChoiceAndScoreLabelsAndRestoresAnswers(t *testing.T) {
	questions := map[string]jev.Question{
		"truth": jev.Noul("is the statement true?"),
		"class": jev.Choice("which class?", map[string]string{
			"none":            "no defect is visible",
			"feature_missing": "an expected feature is absent",
			"low-contrast":    "text cannot be read",
		}),
		"severity": jev.Score("how severe?", []string{
			"cosmetic only, no effect on use",
			"minor annoyance",
			"noticeable slow-down",
			"major blocked task",
			"critical data loss",
		}),
	}
	normalized, aliases := systemOneQuestions(questions)
	for id, question := range normalized {
		encoded, _ := json.Marshal(question.Criteria)
		t.Logf("%s: %s", id, encoded)
	}
	if _, ok := normalized["truth"].Criteria.(map[string]string); !ok {
		t.Fatalf("noul lacks explicit labels: %#v", normalized["truth"].Criteria)
	}
	choice, _ := normalized["class"].Criteria.(map[string]string)
	if choice["none"] != "none" || choice["lowcontrast"] != "lowcontrast" {
		t.Fatalf("choice labels weren't normalized: %#v", choice)
	}
	levels, _ := normalized["severity"].Criteria.([]string)
	if len(levels) != 5 || levels[4] != "level4" {
		t.Fatalf("score levels weren't normalized: %#v", levels)
	}
	result := jev.Result{Answers: map[string]jev.Answer{
		"class":    {ID: "class", Type: "choice", Choice: "featuremissing", Probabilities: map[string]float64{"none": 0.1, "featuremissing": 0.9}},
		"severity": {ID: "severity", Type: "score", Score: 3.5, Probabilities: map[string]float64{"level0": 0, "level1": 0, "level2": 0, "level3": 0.5, "level4": 0.5}},
	}}
	restoreSystemOneAnswers(&result, aliases)
	if got := result.Answers["class"].Choice; got != "feature_missing" {
		t.Fatalf("choice restored to %q", got)
	}
	if got := result.Answers["severity"].Probabilities["3"]; got != 0.5 {
		t.Fatalf("score key wasn't restored: %#v", result.Answers["severity"].Probabilities)
	}
}

func TestSystemOneSplitsMixedQuestionsToAvoidLabelCollision(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		questions := body["questions"].(map[string]any)
		if len(questions) > 1 {
			http.Error(w, `{"error":{"message":"question guidance: label no is not a single token"}}`, http.StatusUnprocessableEntity)
			return
		}
		answers := map[string]any{}
		for id, raw := range questions {
			q := raw.(map[string]any)
			switch q["type"] {
			case "noul":
				answers[id] = map[string]any{"type": "noul", "noul": 0.8}
			case "choice":
				criteria := q["criteria"].(map[string]any)
				keys := []string{}
				for key := range criteria {
					keys = append(keys, key)
				}
				answer := keys[0]
				probs := map[string]float64{}
				for _, key := range keys {
					probs[key] = 0.1
				}
				probs[answer] = 0.9
				answers[id] = map[string]any{"type": "choice", "choice": answer, "probabilities": probs, "confidence": 0.9}
			case "score":
				answers[id] = map[string]any{"type": "score", "score": 1, "probabilities": map[string]float64{"0": 0, "1": 1}, "confidence": 1}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake", "answers": answers, "usage": map[string]any{}})
	}))
	defer server.Close()
	adapter := &openRouterDecisions{client: jev.NewClient(server.URL+"/v1/systemone", "typesafe/jev", "", time.Second)}
	questions := map[string]jev.Question{
		"guidance": jev.Noul("does the dialog lack a way to proceed?"),
		"class":    jev.Choice("which class?", map[string]string{"none": "no defect", "guidance_missing": "missing affordance"}),
	}
	result, err := adapter.Decide(context.Background(), map[string]string{"screen": "x"}, questions, 1, nil)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d, want 2 one-question reads", requests)
	}
	if got := result.Answers["class"].Choice; got != "none" && got != "guidance_missing" {
		t.Fatalf("invalid restored choice %q", got)
	}
	if _, ok := result.Answers["guidance"]; !ok {
		t.Fatal("noul missing")
	}
}

func TestSystemOneNormalizationDoesNotMutateSharedCriteria(t *testing.T) {
	shared := knowledge.ClassCriteria()
	questions := map[string]jev.Question{
		"a": jev.Choice("first persona", shared),
		"b": jev.Choice("second persona", shared),
	}
	var wait sync.WaitGroup
	for index := 0; index < 16; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _ = systemOneQuestions(questions)
		}()
	}
	wait.Wait()
	if _, ok := shared["none"]; !ok {
		t.Fatal("normalizing a System One request mutated the shared class vocabulary")
	}
	if _, ok := shared["clear"]; ok {
		t.Fatal("endpoint-only label leaked into the shared vocabulary")
	}
}
