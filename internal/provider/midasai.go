//go:build midas

package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	ai "github.com/CarlvinceTan/midas/pkg/ai"
	"github.com/CarlvinceTan/midas/pkg/ai/providers/openai"

	"github.com/carlvincetan/argus/internal/config"
)

// This file is the bridge to Midas's own model layer: the LLM tier speaks through
// `pkg/ai`, so Argus uses the same provider adapters, streaming and error
// handling as the rest of the toolchain instead of a private HTTP client.
//
// Two adapters are built on it:
//
//   - MidasVision: a multimodal chat model, used as the vision/semantic tier.
//   - MidasDecisions: the same chat model answering the typed decision questions,
//     for runs where the calibrated tier is unavailable.
//
// The transport is pluggable so an Argus run can point either at a hosted
// gateway or at a local OpenAI-compatible server.

// MidasModel describes which model to call through pkg/ai.
type MidasModel struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
	// AllowNoKey is for local servers that do not check credentials.
	AllowNoKey bool
}

// midasProviderName maps an Argus provider name onto the API style Midas should
// speak. Argus provider names describe where a model lives; Midas's adapters care
// about the wire protocol.
func midasProviderName(provider string) string {
	switch strings.ToLower(provider) {
	case "midasai", "commandcode", "local", "opensource", "ollama":
		return "openai-completions"
	case "openai-responses":
		return "openai-responses"
	default:
		return provider
	}
}

// NewMidasProvider builds an ai.Provider for an OpenAI-compatible endpoint.
func NewMidasProvider(model MidasModel) (ai.Provider, ai.Model, error) {
	if strings.TrimSpace(model.Model) == "" {
		return nil, ai.Model{}, errors.New("midas ai: no model configured")
	}
	model.Provider = midasProviderName(model.Provider)
	switch strings.ToLower(model.Provider) {
	case "", "openai", "openai-completions", "openai-responses", "commandcode", "local", "ollama":
		adapter := openai.New(openai.Options{
			ID:            "argus",
			APIKey:        model.APIKey,
			BaseURL:       model.BaseURL,
			AllowNoAPIKey: model.AllowNoKey || model.APIKey == "",
		})
		api := "openai-completions"
		if strings.EqualFold(model.Provider, "openai-responses") {
			api = "openai-responses"
		}
		selected := ai.Model{
			ID:       model.Model,
			Name:     model.Model,
			Provider: "argus",
			BaseURL:  model.BaseURL,
			API:      api,
			Input:    []ai.Modality{ai.ModalityText, ai.ModalityImage},
		}
		return adapter, selected, nil
	default:
		return nil, ai.Model{}, fmt.Errorf("midas ai: unsupported provider %q", model.Provider)
	}
}

// MidasVision uses pkg/ai as the vision tier: prompt plus images in, text out.
type MidasVision struct {
	provider ai.Provider
	model    ai.Model
	timeout  time.Duration

	mu    sync.Mutex
	calls int
}

// NewMidasVision builds the vision adapter from configuration.
func NewMidasVision(cfg config.Config) (*MidasVision, error) {
	adapter, model, err := NewMidasProvider(MidasModel{
		Provider: cfg.Provider,
		Model:    cfg.VisionModel,
		BaseURL:  cfg.VisionBaseURL,
		APIKey:   cfg.VisionKey,
	})
	if err != nil {
		return nil, err
	}
	timeout := cfg.Limits.VisionTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	return &MidasVision{provider: adapter, model: model, timeout: timeout}, nil
}

func (m *MidasVision) Name() string { return "midas-ai/" + m.model.ID }

func (m *MidasVision) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// Describe sends the prompt with the images and returns the model's text.
func (m *MidasVision) Describe(ctx context.Context, images [][]byte, prompt string, meta map[string]any) (string, error) {
	parts := []ai.Content{ai.NewText(prompt)}
	for _, image := range images {
		parts = append(parts, ai.NewImage(base64.StdEncoding.EncodeToString(image), "image/png"))
	}
	content, err := ai.MultipartUserContent(parts...)
	if err != nil {
		return "", fmt.Errorf("midas ai: building multipart content: %w", err)
	}
	message := ai.UserMessage{Role: ai.RoleUser, Content: content, Timestamp: time.Now().UnixMilli()}
	text, err := m.complete(ctx, ai.Context{Messages: []ai.Message{message}})
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return text, nil
}

// complete streams a context to completion and returns its text.
func (m *MidasVision) complete(ctx context.Context, request ai.Context) (string, error) {
	streamCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	temperature := 0.0
	stream, err := m.provider.Stream(streamCtx, m.model, request, ai.StreamOptions{
		Temperature: &temperature,
		MaxTokens:   1200,
		Reasoning:   ai.ThinkingOff,
	})
	if err != nil {
		return "", fmt.Errorf("midas ai: %w", err)
	}
	text, result, err := collectStreamText(streamCtx, stream)
	if err != nil {
		return "", err
	}
	if result != nil && result.StopReason == ai.StopError && result.ErrorMessage != "" {
		return "", fmt.Errorf("midas ai: %s", result.ErrorMessage)
	}
	return text, nil
}

// collectStreamText drains a stream and returns its text: the deltas as they
// arrive, falling back to the terminal message's content parts. Midas reports
// text through both, and which one carries the answer depends on the provider.
func collectStreamText(ctx context.Context, stream *ai.AssistantStream) (string, *ai.AssistantMessage, error) {
	var builder strings.Builder
	var thinking strings.Builder
	for {
		event, ok, err := stream.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", nil, fmt.Errorf("midas ai: %w", err)
		}
		if !ok {
			break
		}
		// Only user-visible text is collected: reasoning streams through its own
		// thinking_delta events, and appending those produces a transcript of
		// the model's internal monologue instead of its answer.
		switch event.Type {
		case ai.EventTextDelta:
			// Deltas are the text; text_end repeats it, so it is not appended.
			builder.WriteString(event.Delta)
		case ai.EventThinkingDelta, ai.EventThinkingEnd:
			// Kept aside: some reasoning models stream their answer as thinking
			// text, exactly as the HTTP transport's reasoning fallback covers.
			thinking.WriteString(event.Delta)
		case ai.EventDone:
			if builder.Len() == 0 && event.Message != nil {
				builder.WriteString(assistantText(*event.Message))
			}
		}
	}
	result, err := stream.Result(ctx)
	if err != nil && !errors.Is(err, io.EOF) && result.StopReason == "" {
		return "", nil, fmt.Errorf("midas ai: %w", err)
	}
	if builder.Len() == 0 {
		builder.WriteString(assistantText(result))
	}
	if builder.Len() == 0 {
		builder.WriteString(thinking.String())
	}
	return strings.TrimSpace(builder.String()), &result, nil
}

// assistantText joins the text-bearing parts of an assistant message, including
// reasoning content, which is what a reasoning-only reply carries.
func assistantText(message ai.AssistantMessage) string {
	var builder strings.Builder
	for _, part := range message.Content {
		switch typed := part.(type) {
		case ai.TextContent:
			builder.WriteString(typed.Text)
		case ai.ThinkingContent:
			builder.WriteString(typed.Thinking)
		}
	}
	return strings.TrimSpace(builder.String())
}

// midasChatTransport runs the chat-based decision adapter through pkg/ai, so the
// LLM tier uses Midas's provider adapters and streaming rather than a private
// HTTP client.
type midasChatTransport struct {
	provider ai.Provider
	model    ai.Model
	timeout  time.Duration
}

// NewMidasChatTransport builds the transport from configuration.
func NewMidasChatTransport(cfg config.Config) (ChatTransport, error) {
	adapter, model, err := NewMidasProvider(MidasModel{
		Provider: cfg.Provider,
		Model:    cfg.CompatModel,
		BaseURL:  cfg.CompatBaseURL,
		APIKey:   cfg.CompatKey,
	})
	if err != nil {
		return nil, err
	}
	timeout := cfg.Limits.DecisionTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	return &midasChatTransport{provider: adapter, model: model, timeout: timeout}, nil
}

func (t *midasChatTransport) Name() string { return "midas-ai:" + t.model.ID }

func (t *midasChatTransport) Chat(ctx context.Context, system, user string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = t.timeout
	}
	streamCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	temperature := 0.0
	stream, err := t.provider.Stream(streamCtx, t.model, ai.Context{
		SystemPrompt: system,
		Messages:     []ai.Message{ai.NewUserMessage(user, time.Now())},
	}, ai.StreamOptions{
		Temperature: &temperature,
		MaxTokens:   3000,
		Reasoning:   ai.ThinkingOff,
	})
	if err != nil {
		return "", fmt.Errorf("midas ai: %w", err)
	}
	text, result, err := collectStreamText(streamCtx, stream)
	if err != nil {
		return "", err
	}
	if result != nil && result.StopReason == ai.StopError && result.ErrorMessage != "" {
		return "", fmt.Errorf("midas ai: %s", result.ErrorMessage)
	}
	return text, nil
}

func buildMidas(cfg config.Config, wantVision bool) (DecisionProvider, VisionProvider, error) {
	transport, err := NewMidasChatTransport(cfg)
	if err != nil {
		return nil, nil, err
	}
	var vision VisionProvider
	if wantVision {
		vision, err = NewMidasVision(cfg)
		if err != nil {
			return nil, nil, err
		}
	}
	return NewCompatDecisionsWith(transport, cfg.CompatModel), vision, nil
}
