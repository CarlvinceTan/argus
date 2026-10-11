package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/jev"
)

// OpenSourceDecisions is the open-source stand-in for TypeSafe Jev: it asks a
// llama.cpp model *one token* at a time and reads the token distribution instead
// of the model's prose.
//
// That distinction is the whole point. A chat model asked "is this a defect?"
// answers with a sentence, and any probability attached to it is invented by
// code afterwards. Here the answer is a single token whose log-probability the
// server reports, so summing the mass over "yes"/"Yes"/" no" gives a genuine
// distribution over the answers. It is not Jev's calibration, but it is a real
// probability from a real model, self-hosted, with no hosted dependency.
type OpenSourceDecisions struct {
	baseURL string
	model   string
	httpc   *http.Client
	workers int

	mu         sync.Mutex
	calls      int
	tokensIn   int
	transcript []jev.TranscriptEntry
}

// NewOpenSourceDecisions builds the calibrated open-source adapter.
func NewOpenSourceDecisions(cfg config.Config) *OpenSourceDecisions {
	timeout := cfg.Limits.DecisionTimeout
	if timeout < 60*time.Second {
		timeout = 90 * time.Second // one request per question, so be patient
	}
	return &OpenSourceDecisions{
		// llama.cpp serves /completion at the root: the OpenAI-compatible /v1
		// prefix that the chat endpoints use must not be carried over.
		baseURL: strings.TrimSuffix(strings.TrimRight(cfg.LocalBaseURL, "/"), "/v1"),
		model:   cfg.LocalModel,
		httpc:   &http.Client{Timeout: timeout},
		workers: 4,
	}
}

func (o *OpenSourceDecisions) Name() string { return "opensource/llama.cpp-logprobs" }

func (o *OpenSourceDecisions) Calls() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

func (o *OpenSourceDecisions) CostUSD() float64 { return 0 }

func (o *OpenSourceDecisions) Transcript() []jev.TranscriptEntry {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]jev.TranscriptEntry, len(o.transcript))
	copy(out, o.transcript)
	return out
}

// completionRequest is the llama.cpp /completion call, which is the endpoint that
// exposes token log-probabilities (the chat endpoint does not).
type completionRequest struct {
	Prompt      string   `json:"prompt"`
	NPredict    int      `json:"n_predict"`
	NProbs      int      `json:"n_probs"`
	Temperature float64  `json:"temperature"`
	TopK        int      `json:"top_k"`
	TopP        float64  `json:"top_p"`
	CachePrompt bool     `json:"cache_prompt"`
	Stream      bool     `json:"stream"`
	Stop        []string `json:"stop,omitempty"`
}

type logprobEntry struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
}

type completionResponse struct {
	Content       string `json:"content"`
	TokensEval    int    `json:"tokens_evaluated"`
	Probabilities []struct {
		Token       string         `json:"token"`
		Logprob     float64        `json:"logprob"`
		TopLogprobs []logprobEntry `json:"top_logprobs"`
	} `json:"completion_probabilities"`
}

// decide asks one question and returns the token distribution for its answer.
func (o *OpenSourceDecisions) distribution(ctx context.Context, prompt string) (map[string]float64, error) {
	body, err := json.Marshal(completionRequest{
		Prompt:      prompt,
		NPredict:    1,
		NProbs:      24,
		Temperature: 0,
		TopK:        0,
		TopP:        1,
		CachePrompt: true,
		Stream:      false,
		Stop:        []string{"\n"},
	})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/completion", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := o.httpc.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := readLimited(response.Body, 4<<20)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("completion endpoint HTTP %d: %s", response.StatusCode, truncate(string(raw), 200))
	}
	var payload completionResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decoding completion: %w", err)
	}
	o.mu.Lock()
	o.calls++
	o.tokensIn += payload.TokensEval
	o.transcript = append(o.transcript, jev.TranscriptEntry{
		TS:       time.Now(),
		Request:  map[string]any{"prompt_prefix": truncate(prompt, 240)},
		Response: payload,
	})
	o.mu.Unlock()
	if len(payload.Probabilities) == 0 {
		return nil, errors.New("completion returned no probabilities; is the server started with n_probs support?")
	}
	distribution := map[string]float64{}
	for _, entry := range payload.Probabilities[0].TopLogprobs {
		probability := math.Exp(entry.Logprob)
		if probability <= 0 {
			continue
		}
		distribution[entry.Token] = probability
	}
	if len(distribution) == 0 && payload.Probabilities[0].Token != "" {
		distribution[payload.Probabilities[0].Token] = 1
	}
	return distribution, nil
}

// Decide answers every question by reading the model's token distribution.
func (o *OpenSourceDecisions) Decide(
	ctx context.Context,
	state any,
	questions map[string]jev.Question,
	repeats int,
	meta map[string]any,
) (jev.Result, error) {
	if len(questions) == 0 {
		return jev.Result{}, errors.New("no questions supplied")
	}
	started := time.Now()
	stateText, err := renderState(state)
	if err != nil {
		return jev.Result{}, err
	}
	ids := make([]string, 0, len(questions))
	for id := range questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	type outcome struct {
		id     string
		answer jev.Answer
		err    error
	}
	results := make(chan outcome, len(ids))
	semaphore := make(chan struct{}, max(1, o.workers))
	var wait sync.WaitGroup
	for _, id := range ids {
		wait.Add(1)
		go func(id string) {
			defer wait.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			answer, err := o.ask(ctx, id, questions[id], stateText)
			results <- outcome{id: id, answer: answer, err: err}
		}(id)
	}
	wait.Wait()
	close(results)

	answers := map[string]jev.Answer{}
	warnings := []string{}
	for result := range results {
		if result.err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", result.id, result.err))
			continue
		}
		answers[result.id] = result.answer
	}
	if len(answers) == 0 {
		return jev.Result{}, fmt.Errorf("open-source decision provider answered nothing: %s", strings.Join(warnings, "; "))
	}
	return jev.Result{
		Answers:   answers,
		Model:     o.model,
		Usage:     jev.Usage{Local: true, InputTokens: o.tokensIn},
		Latency:   time.Since(started),
		Repeats:   1,
		Warnings:  warnings,
		QuestionN: len(questions),
	}, nil
}

func (o *OpenSourceDecisions) ask(ctx context.Context, id string, question jev.Question, stateText string) (jev.Answer, error) {
	switch question.Type {
	case "noul":
		prompt := fmt.Sprintf(`%s
Question: %s
The answer is a single word: yes or no.
Answer:`, stateText, question.Instructions)
		distribution, err := o.distribution(ctx, prompt)
		if err != nil {
			return jev.Answer{}, err
		}
		yes := massFor(distribution, []string{"yes", "Yes", " yes", " Yes", "YES", "true", "True"})
		no := massFor(distribution, []string{"no", "No", " no", " No", "NO", "false", "False"})
		total := yes + no
		if total <= 0 {
			return jev.Answer{}, fmt.Errorf("no yes/no token in the distribution (top token %q)", topToken(distribution))
		}
		return jev.Answer{ID: id, Type: "noul", Noul: clampProbability(yes / total)}, nil

	case "choice":
		keys, _, err := criteriaKeys(question.Criteria)
		if err != nil {
			return jev.Answer{}, err
		}
		labels := make([]string, 0, len(keys))
		for key := range keys {
			labels = append(labels, key)
		}
		sort.Strings(labels)
		prompt := fmt.Sprintf(`%s
Question: %s
Answer with exactly one of these keys, nothing else: %s
Answer:`, stateText, question.Instructions, strings.Join(labels, ", "))
		distribution, err := o.distribution(ctx, prompt)
		if err != nil {
			return jev.Answer{}, err
		}
		probabilities := map[string]float64{}
		total := 0.0
		for _, label := range labels {
			value := massFor(distribution, []string{label, " " + label, strings.ToUpper(label), strings.Title(label)})
			probabilities[label] = value
			total += value
		}
		if total <= 0 {
			return jev.Answer{}, fmt.Errorf("no criteria token in the distribution (top token %q)", topToken(distribution))
		}
		best, bestValue := "", -1.0
		for label, value := range probabilities {
			probabilities[label] = value / total
			if probabilities[label] > bestValue {
				best, bestValue = label, probabilities[label]
			}
		}
		return jev.Answer{ID: id, Type: "choice", Choice: best, Probabilities: probabilities, Confidence: bestValue}, nil

	case "score":
		criteria, ordered, err := criteriaKeys(question.Criteria)
		if err != nil {
			return jev.Answer{}, err
		}
		_ = criteria
		prompt := fmt.Sprintf(`%s
Question: %s
Levels: %s
Answer with a single digit from 0 to %d.
Answer:`, stateText, question.Instructions, strings.Join(ordered, " | "), len(ordered)-1)
		distribution, err := o.distribution(ctx, prompt)
		if err != nil {
			return jev.Answer{}, err
		}
		probabilities := map[string]float64{}
		total := 0.0
		for level := 0; level < len(ordered); level++ {
			digit := fmt.Sprintf("%d", level)
			value := massFor(distribution, []string{digit, " " + digit})
			probabilities[digit] = value
			total += value
		}
		if total <= 0 {
			return jev.Answer{}, fmt.Errorf("no digit in the distribution (top token %q)", topToken(distribution))
		}
		weighted := 0.0
		bestValue := -1.0
		for level := 0; level < len(ordered); level++ {
			digit := fmt.Sprintf("%d", level)
			probability := probabilities[digit] / total
			probabilities[digit] = probability
			weighted += float64(level) * probability
			if probability > bestValue {
				bestValue = probability
			}
		}
		return jev.Answer{ID: id, Type: "score", Score: weighted, Probabilities: probabilities, Confidence: bestValue}, nil

	default:
		return jev.Answer{}, fmt.Errorf("unsupported question type %q", question.Type)
	}
}

// massFor sums the probability of every listed surface form.
func massFor(distribution map[string]float64, forms []string) float64 {
	total := 0.0
	for _, form := range forms {
		total += distribution[form]
	}
	return total
}

func topToken(distribution map[string]float64) string {
	best, bestValue := "", -1.0
	for token, value := range distribution {
		if value > bestValue {
			best, bestValue = token, value
		}
	}
	return best
}

// renderState flattens the audit state into the prompt prefix. Everything shared
// between questions sits at the front so the server's prompt cache can reuse it.
func renderState(state any) (string, error) {
	switch typed := state.(type) {
	case map[string]any:
		var builder strings.Builder
		if screen, ok := typed["screen"].(map[string]any); ok {
			if text, ok := screen["text"].(string); ok {
				builder.WriteString("SCREEN:\n")
				builder.WriteString(text)
				builder.WriteString("\n")
			}
			if structure, ok := screen["structure"].(string); ok && structure != "" {
				builder.WriteString("STRUCTURE:\n")
				builder.WriteString(structure)
				builder.WriteString("\n")
			}
		}
		if app, ok := typed["app"]; ok {
			encoded, _ := json.Marshal(app)
			fmt.Fprintf(&builder, "APP: %s\n", encoded)
		}
		if motion, ok := typed["motion"]; ok {
			encoded, _ := json.Marshal(motion)
			fmt.Fprintf(&builder, "MOTION: %s\n", truncate(string(encoded), 600))
		}
		if candidates, ok := typed["candidates"]; ok {
			encoded, _ := json.Marshal(candidates)
			fmt.Fprintf(&builder, "CANDIDATES: %s\n", truncate(string(encoded), 900))
		}
		if policy, ok := typed["policy"].(string); ok {
			builder.WriteString("POLICY:\n")
			builder.WriteString(truncate(policy, 1800))
			builder.WriteString("\n")
		}
		return builder.String(), nil
	default:
		encoded, err := json.Marshal(state)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	}
}
