// Package jev is a validating client for the TypeSafe "System One" decision API.
//
// Contract:
//
//	POST <endpoint>
//	{"model": "...", "state": <any>, "questions": {...}, "images": [...]?}
//	-> {"answers": {id: ...}, "usage": {...}, "model": "..."}
//
// Question types are "noul" (P(yes)), "choice" (argmax plus distribution) and
// "score" (probability-weighted level). Every response is validated before use:
// probabilities must match the criteria keys, sum to one, and the chosen option
// must be the argmax. A response that fails validation is an error, never a
// silently accepted value.
//
// The same wire format is served by TypeSafe Jev, by Command Code's System One
// endpoint and by the free DiffusionGemma read endpoint, which is why there is
// one client and an endpoint setting rather than one client per vendor.
package jev

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Question is one typed question about the state.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Noul builds a yes/no question.
func Noul(instructions string) Question {
	return Question{Type: "noul", Instructions: instructions}
}

// Choice builds a single-option question whose criteria map option -> meaning.
func Choice(instructions string, criteria map[string]string) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: criteria}
}

// Score builds an ordered-level question.
func Score(instructions string, levels []string) Question {
	return Question{Type: "score", Instructions: instructions, Criteria: levels}
}

// Answer is one validated answer.
type Answer struct {
	ID            string
	Type          string
	Noul          float64
	Choice        string
	Score         float64
	Probabilities map[string]float64
	Confidence    float64
}

// P returns the primary probability of the answer: the noul value itself, or
// the confidence of a choice/score.
func (a Answer) P() float64 {
	if a.Type == "noul" {
		return a.Noul
	}
	return a.Confidence
}

// Usage reports tokens and cost as returned by the provider. A local or
// key-less endpoint reports zero cost.
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost"`
	Local        bool    `json:"local,omitempty"`
	Degraded     bool    `json:"degraded,omitempty"`
}

// TranscriptEntry is an audit record of one HTTP exchange.
type TranscriptEntry struct {
	TS       time.Time      `json:"ts"`
	Meta     map[string]any `json:"meta,omitempty"`
	Request  any            `json:"request"`
	Response any            `json:"response"`
}

// Result is a completed decision call.
type Result struct {
	Answers   map[string]Answer
	Model     string
	Usage     Usage
	Latency   time.Duration
	Repeats   int
	Cached    bool
	Warnings  []string
	RawFirst  map[string]any
	QuestionN int
}

// Client talks to one decisions endpoint.
type Client struct {
	Endpoint string
	Model    string

	apiKey    string
	httpc     *http.Client
	cacheDir  string
	cacheable bool

	mu         sync.Mutex
	calls      int
	costUSD    float64
	transcript []TranscriptEntry
}

// Option customises a Client.
type Option func(*Client)

// WithCache stores successful responses on disk, keyed by request content.
func WithCache(dir string) Option {
	return func(c *Client) {
		c.cacheDir = dir
		c.cacheable = dir != ""
	}
}

// WithHTTPClient supplies a custom HTTP client (used by tests).
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) { c.httpc = client }
}

// NewClient builds a decisions client.
func NewClient(endpoint, model, apiKey string, timeout time.Duration, opts ...Option) *Client {
	client := &Client{
		Endpoint: endpoint,
		Model:    model,
		apiKey:   apiKey,
		httpc:    &http.Client{Timeout: timeout},
	}
	for _, opt := range opts {
		opt(client)
	}
	return client
}

// Calls returns the number of HTTP requests made.
func (c *Client) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// CostUSD returns the accumulated provider cost.
func (c *Client) CostUSD() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.costUSD
}

// Transcript returns a copy of the audit trail.
func (c *Client) Transcript() []TranscriptEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]TranscriptEntry, len(c.transcript))
	copy(out, c.transcript)
	return out
}

type decisionsRequest struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
	// Images are data URLs. A decision endpoint with a vision tower reads them
	// ahead of the state, which lets the calibrated tier judge a screenshot
	// directly instead of judging a description of one.
	Images []string `json:"images,omitempty"`
}

type decisionsResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   struct {
		InputTokens  int     `json:"input_tokens"`
		OutputTokens int     `json:"output_tokens"`
		Cost         float64 `json:"cost"`
	} `json:"usage"`
}

// Decide asks every question about one state.
func (c *Client) Decide(
	ctx context.Context,
	state any,
	questions map[string]Question,
	repeats int,
	meta map[string]any,
) (Result, error) {
	return c.DecideWithImages(ctx, state, questions, nil, repeats, meta)
}

// DecideWithImages asks every question about a state that includes images.
//
// With repeats > 1 the per-question median (or modal label) is returned so one
// flaky sample cannot decide an outcome.
func (c *Client) DecideWithImages(
	ctx context.Context,
	state any,
	questions map[string]Question,
	images []string,
	repeats int,
	meta map[string]any,
) (Result, error) {
	if len(questions) == 0 {
		return Result{}, errors.New("no questions supplied")
	}
	if repeats < 1 {
		repeats = 1
	}
	body := decisionsRequest{Model: c.Model, State: state, Questions: questions, Images: images}
	encoded, err := json.Marshal(body)
	if err != nil {
		return Result{}, fmt.Errorf("encoding request: %w", err)
	}
	// Opt-in diagnostic capture: never logs by default, never stores credentials,
	// and is useful when a compatible System One endpoint rejects a schema.
	if os.Getenv("ARGUS_DEBUG_SYSTEMONE_REQUEST") == "1" && strings.Contains(c.Endpoint, "/systemone") {
		file, openErr := os.OpenFile("/tmp/argus-systemone-requests.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if openErr == nil {
			_, _ = file.Write(append(encoded, '\n'))
			_ = file.Close()
		}
	}

	payloads := make([]decisionsResponse, 0, repeats)
	usedCache := false
	if cached, ok := c.readCache(encoded); ok {
		payloads = append(payloads, cached)
		usedCache = true
	}
	started := time.Now()
	for len(payloads) < repeats {
		payload, err := c.post(ctx, encoded)
		if err != nil {
			return Result{}, err
		}
		payloads = append(payloads, payload)
		c.writeCache(encoded, payload)
	}
	latency := time.Since(started)

	samples := make(map[string][]Answer, len(questions))
	warnings := []string{}
	for _, payload := range payloads {
		for id := range questions {
			raw, ok := payload.Answers[id]
			if !ok {
				continue
			}
			answer, err := validate(id, questions[id], raw)
			if err != nil {
				// One malformed answer must not discard the whole response: the
				// question is dropped and the caller sees it as unanswered.
				warnings = append(warnings, err.Error())
				continue
			}
			samples[id] = append(samples[id], answer)
		}
	}

	merged := make(map[string]Answer, len(questions))
	for id, question := range questions {
		group := samples[id]
		if len(group) == 0 {
			continue
		}
		merged[id] = mergeSamples(question, group)
	}
	if len(merged) == 0 {
		return Result{}, fmt.Errorf("no question produced a usable answer: %v", warnings)
	}

	usage := Usage{}
	for _, payload := range payloads {
		usage.InputTokens += payload.Usage.InputTokens
		usage.OutputTokens += payload.Usage.OutputTokens
		usage.CostUSD += payload.Usage.Cost
	}
	return Result{
		Answers:   merged,
		Model:     payloads[0].Model,
		Usage:     usage,
		Latency:   latency,
		Repeats:   len(payloads),
		Cached:    usedCache && c.Calls() == 0,
		Warnings:  warnings,
		RawFirst:  map[string]any{"model": payloads[0].Model, "usage": usage},
		QuestionN: len(questions),
	}, nil
}

// mergeSamples reduces repeated answers to one: the median for probabilities,
// the modal label for choices.
func mergeSamples(question Question, samples []Answer) Answer {
	if question.Type == "noul" {
		values := make([]float64, 0, len(samples))
		for _, sample := range samples {
			values = append(values, sample.Noul)
		}
		sort.Float64s(values)
		return Answer{ID: samples[0].ID, Type: "noul", Noul: values[len(values)/2]}
	}
	counts := map[string]int{}
	for _, sample := range samples {
		counts[sample.Choice]++
	}
	best, bestCount := "", -1
	for label, count := range counts {
		if count > bestCount || (count == bestCount && label < best) {
			best, bestCount = label, count
		}
	}
	for _, sample := range samples {
		if sample.Choice == best {
			return sample
		}
	}
	return samples[0]
}

func (c *Client) post(ctx context.Context, encoded []byte) (decisionsResponse, error) {
	var lastErr error
	backoffs := []time.Duration{0, 700 * time.Millisecond, 2 * time.Second}
	for _, backoff := range backoffs {
		if backoff > 0 {
			select {
			case <-ctx.Done():
				return decisionsResponse{}, ctx.Err()
			case <-time.After(backoff):
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(encoded))
		if err != nil {
			return decisionsResponse{}, fmt.Errorf("building request: %w", err)
		}
		request.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			request.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		response, err := c.httpc.Do(request)
		if err != nil {
			lastErr = fmt.Errorf("decisions request: %w", err)
			continue
		}
		payload, err := decodeResponse(response)
		if err != nil {
			lastErr = err
			continue
		}
		c.mu.Lock()
		c.calls++
		c.costUSD += payload.Usage.Cost
		c.transcript = append(c.transcript, TranscriptEntry{
			TS:       time.Now(),
			Meta:     nil,
			Request:  json.RawMessage(encoded),
			Response: payload,
		})
		c.mu.Unlock()
		return payload, nil
	}
	return decisionsResponse{}, fmt.Errorf("decisions endpoint unavailable after retries: %w", lastErr)
}

func decodeResponse(response *http.Response) (decisionsResponse, error) {
	defer response.Body.Close()
	raw, err := readAllLimited(response.Body, 8<<20)
	if err != nil {
		return decisionsResponse{}, fmt.Errorf("reading response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		retryable := response.StatusCode == http.StatusTooManyRequests ||
			response.StatusCode == 529 ||
			response.StatusCode >= 500
		err := &HTTPError{Status: response.StatusCode, Body: string(raw)}
		if retryable {
			return decisionsResponse{}, err
		}
		return decisionsResponse{}, fmt.Errorf("decisions endpoint: %w", err)
	}
	var payload decisionsResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return decisionsResponse{}, fmt.Errorf("decoding response: %w", err)
	}
	if len(payload.Answers) == 0 {
		return decisionsResponse{}, errors.New("decisions response contained no answers")
	}
	return payload, nil
}

// HTTPError marks a non-200 response from the decisions endpoint.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, truncate(e.Body, 200))
}

func (c *Client) readCache(request []byte) (decisionsResponse, bool) {
	if !c.cacheable {
		return decisionsResponse{}, false
	}
	raw, err := os.ReadFile(c.cachePath(request))
	if err != nil {
		return decisionsResponse{}, false
	}
	var payload decisionsResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return decisionsResponse{}, false
	}
	if len(payload.Answers) == 0 {
		return decisionsResponse{}, false
	}
	return payload, true
}

func (c *Client) writeCache(request []byte, payload decisionsResponse) {
	if !c.cacheable {
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if err := os.MkdirAll(c.cacheDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(c.cachePath(request), encoded, 0o644)
}

func (c *Client) cachePath(request []byte) string {
	sum := sha256.Sum256(request)
	return filepath.Join(c.cacheDir, "jev-"+hex.EncodeToString(sum[:16])+".json")
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}

func finiteProbability(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}
