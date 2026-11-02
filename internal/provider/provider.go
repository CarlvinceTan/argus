// Package provider is the composition root for the two reasoning tiers.
//
// One narrow interface each for decisions and vision, with two adapters:
// hosted (OpenRouter: TypeSafe Jev and a free VLM) and local (a loopback
// llama.cpp server). Callers never branch on which adapter is in use; that is
// what lets an offline evaluation exercise the same pipeline as a live run.
package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/jev"
)

// DecisionProvider answers typed questions about a state.
type DecisionProvider interface {
	Name() string
	Calls() int
	CostUSD() float64
	Transcript() []jev.TranscriptEntry
	Decide(
		ctx context.Context,
		state any,
		questions map[string]jev.Question,
		repeats int,
		meta map[string]any,
	) (jev.Result, error)
}

// ImageDecisionProvider is the optional capability of a decision endpoint whose
// model has a vision tower: the same typed questions, asked about a screenshot.
type ImageDecisionProvider interface {
	DecideWithImages(
		ctx context.Context,
		state any,
		questions map[string]jev.Question,
		images []string,
		repeats int,
		meta map[string]any,
	) (jev.Result, error)
}

// VisionProvider describes images in plain language.
type VisionProvider interface {
	Name() string
	Calls() int
	Describe(ctx context.Context, images [][]byte, prompt string, meta map[string]any) (string, error)
}

// Manifest records exactly which models produced a run.
type Manifest struct {
	Provider         string            `json:"provider"`
	DecisionModel    string            `json:"decision_model"`
	DecisionEndpoint string            `json:"decision_endpoint"`
	VisionModel      string            `json:"vision_model,omitempty"`
	Limits           config.Limits     `json:"limits"`
	Thresholds       config.Thresholds `json:"thresholds"`
	CacheEnabled     bool              `json:"cache_enabled"`
}

// Set is the pair of providers plus the manifest describing them.
type Set struct {
	Decisions DecisionProvider
	Vision    VisionProvider
	Manifest  Manifest
}

// Build selects adapters from configuration. This is the only place that knows
// about online versus offline.
func Build(cfg config.Config, wantVision bool) (Set, error) {
	var decisions DecisionProvider
	var vision VisionProvider
	switch cfg.Provider {
	case "none":
		// Measurements only: no model is called and nothing leaves the machine.
		return Set{Manifest: Manifest{Provider: cfg.Provider, Limits: cfg.Limits, Thresholds: cfg.Thresholds}}, nil
	case "local":
		decisions = NewLocalDecisions(cfg)
		if wantVision {
			vision = NewLocalVision(cfg)
		}
	case "midasai":
		var err error
		decisions, vision, err = buildMidas(cfg, wantVision)
		if err != nil {
			return Set{}, err
		}
	case "opensource":
		// The open-source stand-in for Jev: a self-hosted llama.cpp model whose
		// token distribution provides the probabilities.
		decisions = NewOpenSourceDecisions(cfg)
		if wantVision {
			vision = NewLocalVision(cfg)
		}
	case "systemone":
		client := jev.NewClient(cfg.JevEndpoint, cfg.JevModel, cfg.CompatKey, cfg.Limits.DecisionTimeout, jev.WithCache(cfg.CacheDir))
		if !cfg.CacheEnabled {
			client = jev.NewClient(cfg.JevEndpoint, cfg.JevModel, cfg.CompatKey, cfg.Limits.DecisionTimeout)
		}
		decisions = &openRouterDecisions{client: client, labels: decisionLabels("")}
		// System One / DiffusionGemma has a vision tower and is itself the
		// calibrated screenshot reader. A separate hosted VLM is opt-in.
		if wantVision && os.Getenv("ARGUS_SYSTEMONE_USE_HOSTED_VISION") == "1" {
			vision = NewHostedVision(cfg)
		}
	case "jev":
		// TypeSafe Jev through Command Code's System One endpoint: the calibrated
		// decision tier, with the vision tier on the same gateway.
		if cfg.CompatKey == "" {
			return Set{}, errors.New("no commandcode credential found in the local opencode configuration")
		}
		client := jev.NewClient(cfg.JevEndpoint, cfg.JevModel, cfg.CompatKey, cfg.Limits.DecisionTimeout, jev.WithCache(cfg.CacheDir))
		if !cfg.CacheEnabled {
			client = jev.NewClient(cfg.JevEndpoint, cfg.JevModel, cfg.CompatKey, cfg.Limits.DecisionTimeout)
		}
		decisions = &openRouterDecisions{client: client, labels: decisionLabels("")}
		if wantVision {
			vision = NewHostedVision(cfg)
		}
	case "opencode":
		// TypeSafe Jev on OpenCode's Console API: the native System One schema,
		// so labels keep their descriptions and questions share calls.
		if cfg.OpenCodeKey == "" {
			return Set{}, errors.New("no OpenCode key: set OPENCODE_API_KEY or run `opencode auth login`")
		}
		options := []jev.Option{
			jev.WithHeader("User-Agent", "argus/1"),
			jev.WithHeader("x-opencode-session", fmt.Sprintf("argus-%d-%d", os.Getpid(), time.Now().Unix())),
		}
		if cfg.CacheEnabled {
			options = append(options, jev.WithCache(cfg.CacheDir))
		}
		client := jev.NewClient(cfg.JevEndpoint, cfg.JevModel, cfg.OpenCodeKey, cfg.Limits.DecisionTimeout, options...)
		decisions = &openRouterDecisions{client: client, labels: decisionLabels("native")}
	case "commandcode":
		if cfg.CompatKey == "" {
			return Set{}, errors.New("no commandcode credential found in the local opencode configuration")
		}
		decisions = NewCompatDecisions(cfg.CompatBaseURL, cfg.CompatModel, cfg.CompatKey, cfg.Limits.DecisionTimeout)
		if wantVision {
			vision = NewHostedVision(cfg) // hosted chat endpoint, commandcode base URL
		}
	default:
		client := jev.NewClient(cfg.JevEndpoint, cfg.JevModel, cfg.OpenRouter, cfg.Limits.DecisionTimeout, jev.WithCache(cfg.CacheDir))
		if !cfg.CacheEnabled {
			client = jev.NewClient(cfg.JevEndpoint, cfg.JevModel, cfg.OpenRouter, cfg.Limits.DecisionTimeout)
		}
		if cfg.OpenRouter == "" {
			return Set{}, errors.New("no OpenRouter credential: set OPENROUTER_API_KEY")
		}
		decisions = &openRouterDecisions{client: client, labels: decisionLabels("")}
		if wantVision {
			vision = NewHostedVision(cfg)
		}
	}
	manifest := Manifest{
		Provider:         cfg.Provider,
		DecisionModel:    decisions.Name(),
		DecisionEndpoint: cfg.JevEndpoint,
		Limits:           cfg.Limits,
		Thresholds:       cfg.Thresholds,
		CacheEnabled:     cfg.CacheEnabled,
	}
	if vision != nil {
		manifest.VisionModel = vision.Name()
	}
	return Set{Decisions: decisions, Vision: vision, Manifest: manifest}, nil
}

// openRouterDecisions serves TypeSafe Jev through OpenRouter's decisions router.
type openRouterDecisions struct {
	client *jev.Client
	// labels is how this endpoint wants choice and score labels: "native" (Jev's
	// own schema, many questions per call) or "tokens" (single-token labels, one
	// question per call, which DiffusionGemma's System One needs). Empty infers
	// it from the endpoint, as before.
	labels string
}

// decisionLabels is the label dialect for a System One endpoint:
// ARGUS_JEV_LABELS when set, else what the provider knows, else inferred.
func decisionLabels(known string) string {
	switch value := strings.ToLower(strings.TrimSpace(os.Getenv("ARGUS_JEV_LABELS"))); value {
	case "native", "tokens":
		return value
	}
	return known
}

func (o *openRouterDecisions) tokenLabels() bool {
	switch o.labels {
	case "native":
		return false
	case "tokens":
		return true
	}
	return strings.HasSuffix(o.client.Endpoint, "/v1/systemone")
}

func (o *openRouterDecisions) Name() string {
	host := o.client.Endpoint
	if index := strings.Index(host, "//"); index >= 0 {
		host = host[index+2:]
	}
	if index := strings.IndexByte(host, '/'); index >= 0 {
		host = host[:index]
	}
	return "systemone/" + o.client.Model + "@" + host
}

func (o *openRouterDecisions) Calls() int { return o.client.Calls() }

func (o *openRouterDecisions) CostUSD() float64 { return o.client.CostUSD() }

func (o *openRouterDecisions) Transcript() []jev.TranscriptEntry { return o.client.Transcript() }

// maxQuestionsPerCall is the endpoint limit (System One rejects more than 20
// questions in one request), so larger batches are split and merged.
const maxQuestionsPerCall = 20

func (o *openRouterDecisions) Decide(
	ctx context.Context,
	state any,
	questions map[string]jev.Question,
	repeats int,
	meta map[string]any,
) (jev.Result, error) {
	return o.DecideWithImages(ctx, state, questions, nil, repeats, meta)
}

// DecideWithImages sends screenshots to a decision endpoint with a vision
// tower. DiffusionGemma's System One endpoint expects noul's yes/no vocabulary
// as explicit single-token true/false labels; other Jev endpoints keep their
// native schema.
func (o *openRouterDecisions) DecideWithImages(
	ctx context.Context,
	state any,
	questions map[string]jev.Question,
	images []string,
	repeats int,
	meta map[string]any,
) (jev.Result, error) {
	limit := maxQuestionsPerCall
	aliases := systemOneAliases{}
	if o.tokenLabels() {
		questions, aliases = systemOneQuestions(questions)
		// The public diffusion endpoint tokenizes every label; isolate reads so
		// vocabularies from neighbouring questions cannot collide.
		limit = 1
	}
	if len(questions) <= limit {
		result, err := o.client.DecideWithImages(ctx, state, questions, images, repeats, meta)
		if err == nil {
			restoreSystemOneAnswers(&result, aliases)
		}
		return result, err
	}
	return o.chunkedDecideLimit(ctx, state, questions, images, repeats, meta, limit, aliases)
}

func (o *openRouterDecisions) chunkedDecideLimit(
	ctx context.Context,
	state any,
	questions map[string]jev.Question,
	images []string,
	repeats int,
	meta map[string]any,
	limit int,
	aliases systemOneAliases,
) (jev.Result, error) {
	ids := make([]string, 0, len(questions))
	for id := range questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	merged := jev.Result{Answers: map[string]jev.Answer{}}
	started := time.Now()
	for start := 0; start < len(ids); start += limit {
		end := min(start+limit, len(ids))
		batch := map[string]jev.Question{}
		for _, id := range ids[start:end] {
			batch[id] = questions[id]
		}
		partial, err := o.client.DecideWithImages(ctx, state, batch, images, repeats, meta)
		if err != nil {
			return jev.Result{}, err
		}
		for id, answer := range partial.Answers {
			merged.Answers[id] = answer
		}
		merged.Usage.InputTokens += partial.Usage.InputTokens
		merged.Usage.OutputTokens += partial.Usage.OutputTokens
		merged.Usage.CostUSD += partial.Usage.CostUSD
		merged.Warnings = append(merged.Warnings, partial.Warnings...)
		merged.Repeats += partial.Repeats
		if merged.Model == "" {
			merged.Model = partial.Model
		}
	}
	merged.Latency = time.Since(started)
	merged.QuestionN = len(questions)
	restoreSystemOneAnswers(&merged, aliases)
	return merged, nil
}

type systemOneAliases map[string]map[string]string // question id -> endpoint label -> public label

func systemOneQuestions(questions map[string]jev.Question) (map[string]jev.Question, systemOneAliases) {
	out := make(map[string]jev.Question, len(questions))
	aliases := systemOneAliases{}
	for id, question := range questions {
		switch question.Type {
		case "noul":
			if question.Criteria == nil {
				question.Criteria = map[string]string{"true": "true", "false": "false"}
			}
		case "choice":
			if original, ok := question.Criteria.(map[string]string); ok {
				// Choice criteria are often shared/global maps (for example the
				// knowledge class vocabulary). Never mutate them: personas run in
				// parallel, and in-place normalization caused cross-request 422s.
				criteria := make(map[string]string, len(original))
				for key, description := range original {
					criteria[key] = description
				}
				// System One tokenizes every choice label, including its descriptions.
				// Replace descriptions with single tokens and restore the public
				// option names after reading the answer.
				normalized := make(map[string]string, len(criteria))
				aliases[id] = map[string]string{}
				for key := range criteria {
					token := systemOneToken(key)
					normalized[token] = token
					aliases[id][token] = key
				}
				question.Criteria = normalized
			}
		case "score":
			if original, ok := question.Criteria.([]string); ok {
				criteria := append([]string{}, original...)
				normalized := make([]string, len(criteria))
				aliases[id] = map[string]string{}
				for index, label := range criteria {
					token := fmt.Sprintf("level%d", index)
					normalized[index] = token
					aliases[id][token] = strconv.Itoa(index)
					_ = label
				}
				question.Criteria = normalized
			}
		}
		out[id] = question
	}
	return out, aliases
}

func systemOneToken(label string) string {
	var builder strings.Builder
	for _, character := range strings.ToLower(label) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			builder.WriteRune(character)
		}
	}
	token := builder.String()
	if token == "" {
		token = "option"
	}
	return token
}

func restoreSystemOneAnswers(result *jev.Result, aliases systemOneAliases) {
	for id, answer := range result.Answers {
		labels := aliases[id]
		if len(labels) == 0 {
			continue
		}
		if answer.Type == "choice" {
			if original, ok := labels[answer.Choice]; ok {
				answer.Choice = original
			}
			probabilities := map[string]float64{}
			for token, probability := range answer.Probabilities {
				original := token
				if mapped, ok := labels[token]; ok {
					original = mapped
				}
				probabilities[original] = probability
			}
			answer.Probabilities = probabilities
		} else if answer.Type == "score" {
			probabilities := map[string]float64{}
			for token, probability := range answer.Probabilities {
				original := token
				if mapped, ok := labels[token]; ok {
					original = mapped
				}
				probabilities[original] = probability
			}
			answer.Probabilities = probabilities
		}
		result.Answers[id] = answer
	}
}

// HostedVision is a multimodal chat model on any OpenAI-compatible endpoint
// (OpenRouter's free tier, or a gateway such as commandcode).
type HostedVision struct {
	baseURL string
	model   string
	key     string
	httpc   *http.Client

	mu    sync.Mutex
	calls int
}

// NewHostedVision builds the hosted vision adapter.
func NewHostedVision(cfg config.Config) *HostedVision {
	return &HostedVision{
		baseURL: strings.TrimRight(cfg.VisionBaseURL, "/"),
		model:   cfg.VisionModel,
		key:     cfg.VisionKey,
		httpc:   &http.Client{Timeout: cfg.Limits.VisionTimeout},
	}
}

func (o *HostedVision) Name() string { return "hosted/" + o.model }

func (o *HostedVision) Calls() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

func (o *HostedVision) Describe(
	ctx context.Context,
	images [][]byte,
	prompt string,
	meta map[string]any,
) (string, error) {
	return chatWithImages(ctx, o.httpc, o.baseURL, o.model, o.key, images, prompt, func() {
		o.mu.Lock()
		o.calls++
		o.mu.Unlock()
	})
}

// LocalVision talks to a llama.cpp server started with a multimodal projector.
type LocalVision struct {
	baseURL string
	model   string
	httpc   *http.Client

	mu    sync.Mutex
	calls int
}

// NewLocalVision builds the offline vision adapter.
func NewLocalVision(cfg config.Config) *LocalVision {
	return &LocalVision{
		baseURL: strings.TrimRight(cfg.LocalBaseURL, "/"),
		model:   cfg.LocalVisionModel,
		httpc:   &http.Client{Timeout: cfg.Limits.VisionTimeout},
	}
}

func (l *LocalVision) Name() string { return "local/" + l.model }

func (l *LocalVision) Calls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func (l *LocalVision) Describe(
	ctx context.Context,
	images [][]byte,
	prompt string,
	meta map[string]any,
) (string, error) {
	return chatWithImages(ctx, l.httpc, l.baseURL, l.model, "", images, prompt, func() {
		l.mu.Lock()
		l.calls++
		l.mu.Unlock()
	})
}

func chatWithImages(
	ctx context.Context,
	httpc *http.Client,
	baseURL string,
	model string,
	key string,
	images [][]byte,
	prompt string,
	onCall func(),
) (string, error) {
	content := []map[string]any{{"type": "text", "text": prompt}}
	for _, image := range images {
		encoded := base64.StdEncoding.EncodeToString(image)
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:image/png;base64," + encoded},
		})
	}
	body := map[string]any{
		"model":       model,
		"temperature": 0,
		"max_tokens":  900,
		"messages":    []map[string]any{{"role": "user", "content": content}},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("encoding vision request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return "", fmt.Errorf("building vision request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := httpc.Do(request)
	if err != nil {
		return "", fmt.Errorf("vision request: %w", err)
	}
	defer response.Body.Close()
	raw, err := readLimited(response.Body, 8<<20)
	if err != nil {
		return "", fmt.Errorf("reading vision response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vision endpoint HTTP %d: %s", response.StatusCode, truncate(string(raw), 200))
	}
	reply, err := chatContent(raw)
	if err != nil {
		return "", fmt.Errorf("decoding vision response: %w", err)
	}
	onCall()
	return reply, nil
}

const localDecisionSystem = `You inspect the screen of a terminal program and answer questions about what is visible.

You get:
  SCREEN      the screen, row-numbered
  STRUCTURE   derived layout facts (panels, bars, highlighted rows)
  CANDIDATES  things a deterministic checker measured, with the exact values it measured
  POLICY      the expectations a user brings to a UI without being told
  QUESTIONS   each question with its id, type and allowed answers

PROCEDURE
1. Read the whole screen first, then answer each question about it.
2. For a noul question, decide yes/no first and then translate that into a number:
   0.95 = clearly yes, 0.8 = yes, 0.6 = probably, 0.4 = probably not, 0.2 = no, 0.05 = clearly no.
3. For a candidate question, the checker already did the measuring. If the measurement is
   real and nothing on the screen explains it, answer yes. Only answer no when the screen
   shows why the measurement is intended (for example a panel that is deliberately open,
   or a value that is deliberately abbreviated).
4. Judge only what this screen shows. Do not invent defects that are not visible here.
5. Severity: 0 cosmetic, 1 minor, 2 noticeable (slows or misleads), 3 major (blocks a task), 4 critical.

EXAMPLE OF THE SHAPE OF EACH ANSWER
{"answers": {"cand1_real_defect": 0.95, "cand1_class": "truncation", "cand1_severity": 2, "dup_line": 0.2}}

EXAMPLE A (confirm a measured candidate)
A candidate says: a value ends in "..···" while 44 columns of that row are empty. Nothing on
the screen explains it. Correct answer: real_defect 0.95, class "truncation", severity 1.
EXAMPLE B (reject a measured candidate)
A candidate says: a panel border is interrupted. The screen shows the panel's sides are open
by design because the content continues beyond it. Correct answer: real_defect 0.2, class "none".

Reply with JSON only, exactly:
{"answers": {"<question id>": <answer>, ...}}

Answer every question you are given. Strings only for choice answers, numbers for noul and
score answers. No markdown, no commentary, no extra keys.`

// localBatchSize keeps each local request small: a small model produces reliable
// JSON for a handful of questions and starts truncating when asked for thirty.
const localBatchSize = 8

// ChatTransport is the narrow transport a chat-based decision adapter needs. Two
// implementations exist: a direct HTTP call, and Midas's own model layer
// (pkg/ai), so the same decision logic runs over either.
type ChatTransport interface {
	Name() string
	Chat(ctx context.Context, system, user string, timeout time.Duration) (string, error)
}

// httpChatTransport posts to an OpenAI-compatible /chat/completions endpoint.
type httpChatTransport struct {
	baseURL string
	model   string
	key     string
	httpc   *http.Client
}

func (t *httpChatTransport) Name() string { return "http:" + t.model }

func (t *httpChatTransport) Chat(ctx context.Context, system, user string, timeout time.Duration) (string, error) {
	body := map[string]any{
		"model":           t.model,
		"temperature":     0,
		"max_tokens":      3000,
		"response_format": map[string]any{"type": "json_object"},
		"reasoning":       map[string]any{"effort": "none"},
		"messages": []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	client := t.httpc
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	if t.key != "" {
		request.Header.Set("Authorization", "Bearer "+t.key)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	raw, err := readLimited(response.Body, 8<<20)
	response.Body.Close()
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("chat endpoint HTTP %d: %s", response.StatusCode, truncate(string(raw), 200))
	}
	return chatContent(raw)
}

// CompatDecisions emulates the decisions contract on any OpenAI-compatible chat
// endpoint: the loopback llama.cpp server for offline evaluation, or a hosted
// gateway such as commandcode for a stronger non-Jev comparison. It asks for
// point answers instead of full distributions and reconstructs distributions in
// code, which is what a chat model can actually deliver; runs using it are
// reported as compat so they are never confused with calibrated Jev runs.
type CompatDecisions struct {
	transport ChatTransport
	model     string

	mu         sync.Mutex
	calls      int
	transcript []jev.TranscriptEntry
}

// NewCompatDecisions builds a chat-model decision adapter over HTTP.
func NewCompatDecisions(baseURL, model, key string, timeout time.Duration) *CompatDecisions {
	return &CompatDecisions{
		transport: &httpChatTransport{
			baseURL: strings.TrimRight(baseURL, "/"),
			model:   model,
			key:     key,
			httpc:   &http.Client{Timeout: timeout},
		},
		model: model,
	}
}

// NewCompatDecisionsWith builds the adapter over any transport, which is how the
// same decision logic runs through Midas's model layer.
func NewCompatDecisionsWith(transport ChatTransport, model string) *CompatDecisions {
	return &CompatDecisions{transport: transport, model: model}
}

// NewLocalDecisions keeps the loopback configuration in one place.
func NewLocalDecisions(cfg config.Config) *CompatDecisions {
	return NewCompatDecisions(cfg.LocalBaseURL, cfg.LocalModel, "", cfg.Limits.DecisionTimeout)
}

func (l *CompatDecisions) Name() string { return "local/" + l.model }

func (l *CompatDecisions) Calls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func (l *CompatDecisions) CostUSD() float64 { return 0 }

func (l *CompatDecisions) Transcript() []jev.TranscriptEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]jev.TranscriptEntry, len(l.transcript))
	copy(out, l.transcript)
	return out
}

// Decide asks the local model in small batches and rebuilds typed answers.
func (l *CompatDecisions) Decide(
	ctx context.Context,
	state any,
	questions map[string]jev.Question,
	repeats int,
	meta map[string]any,
) (jev.Result, error) {
	if len(questions) == 0 {
		return jev.Result{}, errors.New("no questions supplied")
	}
	ids := make([]string, 0, len(questions))
	for id := range questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	started := time.Now()
	answers := map[string]jev.Answer{}
	degraded := false
	failures := 0
	for start := 0; start < len(ids); start += localBatchSize {
		if ctx.Err() != nil {
			return jev.Result{}, ctx.Err()
		}
		end := min(start+localBatchSize, len(ids))
		batch := map[string]jev.Question{}
		for _, id := range ids[start:end] {
			batch[id] = questions[id]
		}
		user := buildLocalPrompt(state, batch)
		pointAnswers, rawReply, err := l.askBatch(ctx, user, meta)
		if err != nil {
			// One retry with a blunter instruction: a chat model that answers in
			// prose usually complies when the format rule is the only rule left.
			pointAnswers, rawReply, err = l.askBatch(ctx, user+"\n\nReply with the JSON object only. No prose.", meta)
			if err != nil {
				failures++
				if failures == len(ids)/localBatchSize+1 {
					return jev.Result{}, fmt.Errorf("local decision provider failed on every batch: %w", err)
				}
				continue
			}
		}
		for id, raw := range pointAnswers {
			question, ok := questions[id]
			if !ok {
				continue
			}
			answer, ok := pointToAnswer(id, question, raw)
			if !ok {
				continue
			}
			if question.Type != "noul" {
				degraded = true // distributions are reconstructed, not modelled
			}
			answers[id] = answer
		}
		_ = rawReply
	}
	if len(answers) == 0 {
		return jev.Result{}, errors.New("local decision provider returned no usable answers")
	}
	return jev.Result{
		Answers:   answers,
		Model:     l.model,
		Usage:     jev.Usage{Local: true, Degraded: degraded},
		Latency:   time.Since(started),
		Repeats:   1,
		QuestionN: len(questions),
	}, nil
}

// buildLocalPrompt renders the screen and questions as plain text. Small models
// handle this far better than nested JSON, and it keeps the question wording
// (which carries the judgement rules) intact.
func buildLocalPrompt(state any, questions map[string]jev.Question) string {
	var b strings.Builder
	typed, isMap := state.(map[string]any)
	if _, isScreen := typed["screen"]; isMap && !isScreen {
		// Not a screen (the change filter, for one): every key is the evidence,
		// so none may be dropped by the screen layout below.
		b.WriteString("STATE:\n")
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if encoded, err := json.Marshal(typed[key]); err == nil {
				fmt.Fprintf(&b, "\n%s: %s\n", strings.ToUpper(key), string(encoded))
			}
		}
		state = nil
	} else {
		b.WriteString("SCREEN:\n")
	}
	switch typed := state.(type) {
	case nil:
	case map[string]any:
		if screen, ok := typed["screen"].(map[string]any); ok {
			if text, ok := screen["text"].(string); ok {
				b.WriteString(text)
				if !strings.HasSuffix(text, "\n") {
					b.WriteString("\n")
				}
			}
			if structure, ok := screen["structure"].(string); ok && structure != "" {
				b.WriteString("\nSTRUCTURE:\n")
				b.WriteString(structure)
			}
		}
		for _, key := range []string{"app", "action", "motion", "candidates", "policy"} {
			if value, ok := typed[key]; ok {
				encoded, err := json.Marshal(value)
				if err == nil {
					fmt.Fprintf(&b, "\n%s: %s\n", strings.ToUpper(key), string(encoded))
				}
			}
		}
	default:
		encoded, err := json.Marshal(state)
		if err == nil {
			b.WriteString(string(encoded))
			b.WriteString("\n")
		}
	}

	ids := make([]string, 0, len(questions))
	for id := range questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	b.WriteString("\nQUESTIONS:\n")
	for _, id := range ids {
		question := questions[id]
		fmt.Fprintf(&b, "%s [%s] %s\n", id, question.Type, question.Instructions)
		switch typed := question.Criteria.(type) {
		case map[string]string:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, key := range keys {
				parts = append(parts, fmt.Sprintf("%q (%s)", key, typed[key]))
			}
			fmt.Fprintf(&b, "  options: %s -> answer with one key\n", strings.Join(parts, "; "))
		case []string:
			fmt.Fprintf(&b, "  levels 0..%d: %s -> answer with a whole number\n", len(typed)-1, strings.Join(typed, " | "))
		default:
			b.WriteString("  answer: a number from 0 to 1\n")
		}
	}
	return b.String()
}

// askBatch sends one batch through the transport and parses the point answers.
func (l *CompatDecisions) askBatch(ctx context.Context, user string, meta map[string]any) (map[string]json.RawMessage, string, error) {
	reply, err := l.transport.Chat(ctx, localDecisionSystem, user, 0)
	if err != nil {
		return nil, "", err
	}
	content := stripFences(reply)
	var payload struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		extracted, ok := extractJSONObject(content)
		if !ok {
			return nil, content, fmt.Errorf("model returned unparseable JSON: %w (%s)", err, truncate(content, 160))
		}
		if err := json.Unmarshal([]byte(extracted), &payload); err != nil {
			return nil, content, fmt.Errorf("model returned unparseable JSON: %w (%s)", err, truncate(content, 160))
		}
	}
	if len(payload.Answers) == 0 {
		return nil, content, errors.New("model returned no answers")
	}
	l.mu.Lock()
	l.calls++
	l.transcript = append(l.transcript, jev.TranscriptEntry{
		TS:       time.Now(),
		Meta:     meta,
		Request:  map[string]any{"batch": truncate(user, 200)},
		Response: payload,
	})
	l.mu.Unlock()
	return payload.Answers, content, nil
}

// pointToAnswer converts a point answer (number or label) into a typed answer,
// synthesising a distribution from an assumed confidence.
func pointToAnswer(id string, question jev.Question, raw json.RawMessage) (jev.Answer, bool) {
	const assumedConfidence = 0.7
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if question.Type == "noul" {
			if value, ok := parseProbability(text); ok {
				return jev.Answer{ID: id, Type: "noul", Noul: clampProbability(value)}, true
			}
			switch strings.ToLower(strings.TrimSpace(text)) {
			case "yes", "true":
				return jev.Answer{ID: id, Type: "noul", Noul: 0.8}, true
			case "no", "false":
				return jev.Answer{ID: id, Type: "noul", Noul: 0.2}, true
			}
			return jev.Answer{}, false
		}
		keys, ordered, err := criteriaKeys(question.Criteria)
		if err != nil {
			return jev.Answer{}, false
		}
		match := matchKey(text, keys)
		if match == "" {
			return jev.Answer{}, false
		}
		if question.Type == "score" {
			index := indexOf(ordered, match)
			if index < 0 {
				return jev.Answer{}, false
			}
			return jev.Answer{
				ID:            id,
				Type:          "score",
				Score:         float64(index),
				Probabilities: distribution(ordered, match, assumedConfidence),
				Confidence:    assumedConfidence,
			}, true
		}
		return jev.Answer{
			ID:            id,
			Type:          "choice",
			Choice:        match,
			Probabilities: distribution(ordered, match, assumedConfidence),
			Confidence:    assumedConfidence,
		}, true
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		switch question.Type {
		case "noul":
			// A chat model answering 1.0 is expressing style, not certainty.
			// Clamping keeps a point answer from outweighing measured evidence
			// once it reaches the same thresholds Jev's calibrated numbers use.
			return jev.Answer{ID: id, Type: "noul", Noul: clampProbability(number)}, true
		case "score":
			_, ordered, err := criteriaKeys(question.Criteria)
			if err != nil {
				return jev.Answer{}, false
			}
			level := int(math.Round(number))
			if level < 0 {
				level = 0
			}
			if level > len(ordered)-1 {
				level = len(ordered) - 1
			}
			label := ordered[level]
			return jev.Answer{
				ID:            id,
				Type:          "score",
				Score:         float64(level),
				Probabilities: distribution(ordered, label, assumedConfidence),
				Confidence:    assumedConfidence,
			}, true
		}
		return jev.Answer{}, false
	}
	// Tolerate {"noul": 0.8} / {"choice": "x"} / {"score": 2} shapes.
	var envelope struct {
		Noul   *float64 `json:"noul"`
		Choice string   `json:"choice"`
		Score  *float64 `json:"score"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return jev.Answer{}, false
	}
	if envelope.Noul != nil && question.Type == "noul" {
		return jev.Answer{ID: id, Type: "noul", Noul: *envelope.Noul}, true
	}
	if envelope.Choice != "" {
		rebuilt, err := json.Marshal(envelope.Choice)
		if err == nil {
			return pointToAnswer(id, question, rebuilt)
		}
	}
	if envelope.Score != nil && question.Type == "score" {
		rebuilt, err := json.Marshal(*envelope.Score)
		if err == nil {
			return pointToAnswer(id, question, rebuilt)
		}
	}
	return jev.Answer{}, false
}

// clampProbability keeps point answers inside a band that leaves room for doubt.
func clampProbability(value float64) float64 {
	const floor, ceiling = 0.05, 0.95
	if value < floor {
		return floor
	}
	if value > ceiling {
		return ceiling
	}
	return value
}

func parseProbability(text string) (float64, bool) {
	trimmed := strings.TrimSpace(text)
	if strings.HasSuffix(trimmed, "%") {
		trimmed = strings.TrimSuffix(trimmed, "%")
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(trimmed), 64)
	if err != nil {
		return 0, false
	}
	if strings.Contains(text, "%") {
		value /= 100
	}
	if value < 0 || value > 1 {
		return 0, false
	}
	return value, true
}

func criteriaKeys(criteria any) (map[string]bool, []string, error) {
	switch typed := criteria.(type) {
	case map[string]string:
		keys := map[string]bool{}
		ordered := make([]string, 0, len(typed))
		for label := range typed {
			keys[label] = true
			ordered = append(ordered, label)
		}
		sort.Strings(ordered)
		return keys, ordered, nil
	case []string:
		keys := map[string]bool{}
		for _, label := range typed {
			keys[label] = true
		}
		return keys, append([]string{}, typed...), nil
	case []any:
		keys := map[string]bool{}
		ordered := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, nil, errors.New("criteria must be strings")
			}
			keys[text] = true
			ordered = append(ordered, text)
		}
		return keys, ordered, nil
	case map[string]any:
		keys := map[string]bool{}
		ordered := make([]string, 0, len(typed))
		for label := range typed {
			keys[label] = true
			ordered = append(ordered, label)
		}
		sort.Strings(ordered)
		return keys, ordered, nil
	default:
		return nil, nil, fmt.Errorf("unsupported criteria type %T", criteria)
	}
}

// matchKey maps a model-written label onto a real criteria key.
func matchKey(text string, keys map[string]bool) string {
	cleaned := strings.ToLower(strings.TrimSpace(strings.Trim(text, "\"'`.")))
	if keys[cleaned] {
		return cleaned
	}
	best, bestScore := "", 0.0
	for key := range keys {
		lower := strings.ToLower(key)
		switch {
		case lower == cleaned:
			return key
		case strings.Contains(lower, cleaned) || strings.Contains(cleaned, lower):
			score := float64(len(lower)) / float64(max(len(cleaned), 1))
			if score > bestScore {
				best, bestScore = key, score
			}
		}
	}
	return best
}

func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}

func distribution(ordered []string, selected string, confidence float64) map[string]float64 {
	out := make(map[string]float64, len(ordered))
	rest := 0.0
	if len(ordered) > 1 {
		rest = (1 - confidence) / float64(len(ordered)-1)
	}
	for _, label := range ordered {
		if label == selected {
			out[label] = confidence
			continue
		}
		out[label] = rest
	}
	return out
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(reader, limit))
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}

var fencePattern = regexp.MustCompile("(?m)^```(?:json)?")

// stripFences removes markdown code fences that chat models add around JSON.
func stripFences(text string) string {
	cleaned := fencePattern.ReplaceAllString(text, "")
	return strings.ReplaceAll(cleaned, "```", "")
}

// extractJSONObject finds the outermost JSON object in a reply that also
// contains prose, which is common when a model reasons before answering.
func extractJSONObject(text string) (string, bool) {
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end <= start {
		return "", false
	}
	return text[start : end+1], true
}

// chatContent pulls the assistant text out of a chat completion, falling back to
// the reasoning field some gateways use when the token budget is spent before
// any user-visible content is produced.
func chatContent(raw []byte) (string, error) {
	var envelope struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", fmt.Errorf("decoding chat response: %w", err)
	}
	if len(envelope.Choices) == 0 {
		return "", errors.New("chat response had no choices")
	}
	message := envelope.Choices[0].Message
	if strings.TrimSpace(message.Content) != "" {
		return message.Content, nil
	}
	return message.Reasoning, nil
}

// extractAnswersFromReasoning looks for an answers object inside a reasoning
// field: most gateways put the plan there and the final JSON in content, but
// occasionally the reverse happens, and a decision run should not fail because
// the answer arrived in the other field.
func extractAnswersFromReasoning(raw []byte) map[string]json.RawMessage {
	var envelope struct {
		Choices []struct {
			Message struct {
				Reasoning string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Choices) == 0 {
		return nil
	}
	text := envelope.Choices[0].Message.Reasoning
	start := strings.Index(text, "{\"answers\"")
	if start < 0 {
		start = strings.Index(text, "{'answers'")
	}
	if start < 0 {
		return nil
	}
	candidate, ok := extractJSONObject(text[start:])
	if !ok {
		return nil
	}
	var payload struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal([]byte(candidate), &payload); err != nil {
		return nil
	}
	return payload.Answers
}
