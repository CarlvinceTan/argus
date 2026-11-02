package provider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/jev"
	"github.com/carlvincetan/argus/internal/polymux"
	"github.com/carlvincetan/argus/internal/provider"
)

// Every decision backend Argus can build must take the same typed questions
// (noul, choice, score) about any state, hand the whole state to its model, and
// return answers the rest of Argus can use. These fakes speak each wire format:
// System One (Jev's native labels, or DiffusionGemma's single-token labels),
// OpenAI chat (a chat model emulating decisions) and llama.cpp logprobs. Each
// fake rates every yes/no question 10% and picks the first option and the top
// level, so the answers are predictable whichever backend relays them.

const marker = "STATE-MARKER-7731"

type fake struct {
	mu       sync.Mutex
	requests int
	sawState bool
	headers  http.Header
}

func (f *fake) note(r *http.Request, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	f.sawState = f.sawState || strings.Contains(body, marker)
	f.headers = r.Header.Clone()
}

func systemOneServer(t *testing.T, f *fake) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.note(r, string(raw))
		var request struct {
			Questions map[string]jev.Question `json:"questions"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Errorf("system one request: %v", err)
		}
		answers := map[string]any{}
		for id, question := range request.Questions {
			switch question.Type {
			case "noul":
				answers[id] = map[string]any{"noul": 0.1}
			case "choice":
				keys := []string{}
				for key := range question.Criteria.(map[string]any) {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				probabilities := map[string]float64{}
				for _, key := range keys {
					probabilities[key] = 0.3 / float64(len(keys)-1)
				}
				probabilities[keys[0]] = 0.7
				answers[id] = map[string]any{"choice": keys[0], "probabilities": probabilities, "confidence": 0.7}
			case "score":
				levels := question.Criteria.([]any)
				probabilities := map[string]float64{}
				for index := range levels {
					probabilities[fmt.Sprint(index)] = 0.2 / float64(len(levels)-1)
				}
				top := len(levels) - 1
				probabilities[fmt.Sprint(top)] = 0.8
				answers[id] = map[string]any{"score": float64(top), "probabilities": probabilities, "confidence": 0.8}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake", "answers": answers, "usage": map[string]any{"input_tokens": 100}})
	}))
}

var (
	chatQuestion = regexp.MustCompile(`(?m)^(\S+) \[(noul|choice|score)\]`)
	chatOptions  = regexp.MustCompile(`options: "([^"]+)"`)
	chatLevels   = regexp.MustCompile(`levels 0\.\.(\d+)`)
)

func chatServer(t *testing.T, f *fake) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.note(r, string(raw))
		var request struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.Unmarshal(raw, &request)
		prompt := request.Messages[len(request.Messages)-1].Content
		answers := map[string]any{}
		lines := strings.Split(prompt, "\n")
		for index, line := range lines {
			match := chatQuestion.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			hint := ""
			if index+1 < len(lines) {
				hint = lines[index+1]
			}
			switch match[2] {
			case "noul":
				answers[match[1]] = 0.1
			case "choice":
				answers[match[1]] = chatOptions.FindStringSubmatch(hint)[1]
			case "score":
				var top int
				fmt.Sscan(chatLevels.FindStringSubmatch(hint)[1], &top)
				answers[match[1]] = top
			}
		}
		content, _ := json.Marshal(map[string]any{"answers": answers})
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(content)}}}})
	}))
}

var (
	llamaKeys  = regexp.MustCompile(`one of these keys, nothing else: (.+)\n`)
	llamaDigit = regexp.MustCompile(`single digit from 0 to (\d+)`)
)

func llamaServer(t *testing.T, f *fake) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.note(r, string(raw))
		var request struct{ Prompt string }
		_ = json.Unmarshal(raw, &request)
		top := map[string]float64{}
		switch {
		case strings.Contains(request.Prompt, "yes or no"):
			top["no"], top["yes"] = 0.9, 0.1
		case llamaKeys.MatchString(request.Prompt):
			keys := strings.Split(llamaKeys.FindStringSubmatch(request.Prompt)[1], ", ")
			top[keys[0]], top[keys[1]] = 0.7, 0.3
		case llamaDigit.MatchString(request.Prompt):
			top[llamaDigit.FindStringSubmatch(request.Prompt)[1]], top["0"] = 0.8, 0.2
		}
		entries := []map[string]any{}
		for token, p := range top {
			entries = append(entries, map[string]any{"token": token, "logprob": math.Log(p)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"content": "x", "tokens_evaluated": 10,
			"completion_probabilities": []any{map[string]any{"token": "x", "logprob": 0, "top_logprobs": entries}}})
	}))
}

type backend struct {
	name     string
	server   func(*testing.T, *fake) *httptest.Server
	setup    func(cfg *config.Config, url string)
	labels   string // ARGUS_JEV_LABELS
	requests int    // for the three typed questions; 0 means any
}

var backends = []backend{
	{name: "opencode", server: systemOneServer, requests: 1, setup: func(c *config.Config, url string) {
		c.JevEndpoint, c.OpenCodeKey = url+"/inference/systemone/v1/systemone", "k"
	}},
	{name: "openrouter", server: systemOneServer, requests: 1, setup: func(c *config.Config, url string) {
		c.JevEndpoint, c.OpenRouter = url+"/api/alpha/decisions", "k"
	}},
	{name: "jev", server: systemOneServer, requests: 3, setup: func(c *config.Config, url string) {
		c.JevEndpoint, c.CompatKey = url+"/provider/v1/systemone", "k"
	}},
	{name: "jev", labels: "native", server: systemOneServer, requests: 1, setup: func(c *config.Config, url string) {
		c.JevEndpoint, c.CompatKey = url+"/provider/v1/systemone", "k"
	}},
	{name: "systemone", server: systemOneServer, requests: 3, setup: func(c *config.Config, url string) {
		c.JevEndpoint = url + "/v1/systemone"
	}},
	{name: "commandcode", server: chatServer, requests: 1, setup: func(c *config.Config, url string) {
		c.CompatBaseURL, c.CompatModel, c.CompatKey = url, "m", "k"
	}},
	{name: "local", server: chatServer, requests: 1, setup: func(c *config.Config, url string) {
		c.LocalBaseURL, c.LocalModel = url, "m"
	}},
	{name: "opensource", server: llamaServer, requests: 3, setup: func(c *config.Config, url string) {
		c.LocalBaseURL, c.LocalModel = url+"/v1", "m"
	}},
}

func build(t *testing.T, b backend, f *fake) provider.DecisionProvider {
	t.Helper()
	t.Setenv("ARGUS_JEV_LABELS", b.labels)
	t.Setenv("ARGUS_JEV_ENDPOINT", "")
	server := b.server(t, f)
	t.Cleanup(server.Close)
	cfg := config.Default()
	cfg.Provider = b.name
	cfg.ApplyProviderDefaults()
	cfg.CacheEnabled = false
	b.setup(&cfg, server.URL)
	set, err := provider.Build(cfg, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return set.Decisions
}

func TestEveryDecisionBackendAnswersEveryQuestionType(t *testing.T) {
	levels := []string{"Calm", "Frustrated", "Very angry"}
	options := map[string]string{"billing": "Charges or invoices", "returns": "Refunds or damaged items", "shipping": "Delays"}
	questions := map[string]jev.Question{
		"urgent":      jev.Noul("Does this need urgent attention?"),
		"department":  jev.Choice("Which team should handle it?", options),
		"frustration": jev.Score("How frustrated is the customer?", levels),
	}
	for _, b := range backends {
		t.Run(b.name+"/"+b.labels, func(t *testing.T) {
			f := &fake{}
			decisions := build(t, b, f)
			result, err := decisions.Decide(context.Background(), map[string]any{"message": marker + " my order arrived damaged"}, questions, 1, nil)
			if err != nil {
				t.Fatalf("decide: %v", err)
			}
			if !f.sawState {
				t.Fatal("the model never saw the state")
			}
			if b.requests > 0 && f.requests != b.requests {
				t.Errorf("%d requests, want %d", f.requests, b.requests)
			}
			urgent := result.Answers["urgent"]
			if urgent.Type != "noul" || urgent.P() < 0 || urgent.P() > 0.2 {
				t.Errorf("noul: %+v", urgent)
			}
			department := result.Answers["department"]
			if _, ok := options[department.Choice]; department.Type != "choice" || !ok || department.Choice != "billing" {
				t.Errorf("choice: %+v", department)
			}
			for label := range department.Probabilities {
				if _, ok := options[label]; !ok {
					t.Errorf("choice probability for a label nobody offered: %q", label)
				}
			}
			frustration := result.Answers["frustration"]
			if frustration.Type != "score" || frustration.Score < 1.5 || frustration.Score > float64(len(levels)-1) {
				t.Errorf("score: %+v", frustration)
			}
		})
	}
}

func TestTheChangeFilterRunsOnEveryDecisionBackend(t *testing.T) {
	journeys := []polymux.Journey{{ID: "chat", Flows: []string{"a.js"}}, {ID: "profile", Flows: []string{"b.js"}}, {ID: "sidebar", Flows: []string{"c.js"}}}
	change := polymux.Change{Base: "abc", Files: []string{"Views/Chat.swift"}, Diff: "+// " + marker + "\n"}
	for _, b := range backends {
		t.Run(b.name+"/"+b.labels, func(t *testing.T) {
			f := &fake{}
			decisions := build(t, b, f)
			selection := polymux.DecideJourneys(context.Background(), t.TempDir(), journeys, change, decisions)
			if !f.sawState {
				t.Fatal("the model never saw the diff")
			}
			// Every backend rates each journey 10%, under the 20% bar.
			if len(selection.Kept) != 0 || len(selection.LeftOut) != 3 {
				t.Fatalf("kept %d, left out %v; %s", len(selection.Kept), selection.LeftOut, selection.Note)
			}
		})
	}
}

func TestOpenCodeSendsItsRoutingHeaders(t *testing.T) {
	f := &fake{}
	decisions := build(t, backends[0], f)
	if _, err := decisions.Decide(context.Background(), map[string]any{"m": marker}, map[string]jev.Question{"q": jev.Noul("ok?")}, 1, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.headers.Get("Authorization"); got != "Bearer k" {
		t.Errorf("authorization %q", got)
	}
	if !strings.HasPrefix(f.headers.Get("x-opencode-session"), "argus-") || f.headers.Get("User-Agent") != "argus/1" {
		t.Errorf("routing headers %v", f.headers)
	}
}
