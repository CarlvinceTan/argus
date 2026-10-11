package jev

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateNoul(t *testing.T) {
	question := Noul("is it broken")
	answer, err := Validate("q1", question, json.RawMessage(`{"noul":0.82}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if answer.P() != 0.82 {
		t.Fatalf("P() = %v, want 0.82", answer.P())
	}
	for _, raw := range []string{`{"noul":1.4}`, `{"noul":-0.2}`, `{}`, `{"noul":"high"}`} {
		if _, err := Validate("q1", question, json.RawMessage(raw)); err == nil {
			t.Fatalf("expected rejection for %s", raw)
		}
	}
}

func TestValidateChoice(t *testing.T) {
	question := Choice("which", map[string]string{"none": "no", "truncation": "cut off"})
	good := json.RawMessage(`{"choice":"truncation","probabilities":{"none":0.1,"truncation":0.9},"confidence":0.8}`)
	if _, err := Validate("q1", question, good); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cases := map[string]string{
		"not argmax":       `{"choice":"none","probabilities":{"none":0.1,"truncation":0.9},"confidence":0.8}`,
		"missing label":    `{"choice":"truncation","probabilities":{"truncation":1.0},"confidence":0.8}`,
		"bad sum":          `{"choice":"truncation","probabilities":{"none":0.1,"truncation":0.6},"confidence":0.8}`,
		"outside criteria": `{"choice":"other","probabilities":{"none":0.5,"truncation":0.5},"confidence":0.8}`,
	}
	for name, raw := range cases {
		if _, err := Validate("q1", question, json.RawMessage(raw)); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
}

func TestValidateScore(t *testing.T) {
	question := Score("severity", []string{"none", "minor", "major"})
	raw := json.RawMessage(`{"score":1.4,"probabilities":{"0":0.1,"1":0.5,"2":0.4},"confidence":0.7}`)
	answer, err := Validate("q1", question, raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if answer.Score != 1.4 {
		t.Fatalf("score = %v", answer.Score)
	}
	if _, err := Validate("q1", question, json.RawMessage(`{"score":5,"probabilities":{"0":0.1,"1":0.5,"2":0.4},"confidence":0.7}`)); err == nil {
		t.Fatal("expected score above legend to be rejected")
	}
}

func TestDecideMergesRepeatsAndCaches(t *testing.T) {
	var requests atomic.Int32
	responses := []string{
		`{"model":"fake","answers":{"defect":{"noul":0.9}},"usage":{"input_tokens":10,"cost":0.0001}}`,
		`{"model":"fake","answers":{"defect":{"noul":0.7}},"usage":{"input_tokens":10,"cost":0.0001}}`,
		`{"model":"fake","answers":{"defect":{"noul":0.8}},"usage":{"input_tokens":10,"cost":0.0001}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(requests.Add(1)) - 1
		w.Header().Set("Content-Type", "application/json")
		if index < len(responses) {
			_, _ = w.Write([]byte(responses[index]))
			return
		}
		_, _ = w.Write([]byte(responses[len(responses)-1]))
	}))
	defer server.Close()

	cacheDir := t.TempDir()
	client := NewClient(server.URL, "fake", "", 5*time.Second, WithCache(cacheDir))
	result, err := client.Decide(
		context.Background(),
		map[string]any{"screen": "x"},
		map[string]Question{"defect": Noul("broken?")},
		3,
		nil,
	)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if got := result.Answers["defect"].Noul; got != 0.8 {
		t.Fatalf("median = %v, want 0.8", got)
	}
	if requests.Load() != 3 {
		t.Fatalf("requests = %d, want 3", requests.Load())
	}
	if result.Usage.CostUSD <= 0 {
		t.Fatalf("cost not accumulated: %+v", result.Usage)
	}

	// A second client sharing the cache should not touch the server.
	second := NewClient(server.URL, "fake", "", 5*time.Second, WithCache(cacheDir))
	cached, err := second.Decide(
		context.Background(),
		map[string]any{"screen": "x"},
		map[string]Question{"defect": Noul("broken?")},
		1,
		nil,
	)
	if err != nil {
		t.Fatalf("cached decide: %v", err)
	}
	if requests.Load() != 3 {
		t.Fatalf("cache miss: requests = %d", requests.Load())
	}
	if !cached.Cached {
		t.Fatal("expected cached result to be marked")
	}
}

func TestDecideRejectsInvalidAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"fake","answers":{"defect":{"noul":2.0}},"usage":{}}`))
	}))
	defer server.Close()
	client := NewClient(server.URL, "fake", "", 5*time.Second)
	_, err := client.Decide(
		context.Background(),
		map[string]any{"screen": "x"},
		map[string]Question{"defect": Noul("broken?")},
		1,
		nil,
	)
	if err == nil {
		t.Fatal("expected invalid answer to be rejected")
	}
}

func TestDecideReportsMissingQuestion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"fake","answers":{"other":{"noul":0.5}},"usage":{}}`))
	}))
	defer server.Close()
	client := NewClient(server.URL, "fake", "", 5*time.Second)
	_, err := client.Decide(
		context.Background(),
		map[string]any{"screen": "x"},
		map[string]Question{"defect": Noul("broken?")},
		1,
		nil,
	)
	if err == nil {
		t.Fatal("expected missing question to be an error")
	}
}
