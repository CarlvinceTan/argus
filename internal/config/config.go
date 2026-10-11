// Package config holds runtime configuration and credential discovery.
//
// The same binary runs in two modes: "openrouter" (hosted TypeSafe Jev plus a
// hosted VLM) for development, and "local" (a loopback llama.cpp server) for
// offline evaluation. Nothing downstream branches on the mode; only the
// composition root in provider.Build does.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	OpenRouterAPI = "https://openrouter.ai/api"
	TypeSafeAPI   = "https://api.typesafe.ai/v1/systemone"

	DefaultJevModel     = "~typesafe/jev-latest"
	DefaultVisionModel  = "qwen/qwen3.8-27b:free"
	DefaultLocalBaseURL = "http://127.0.0.1:8080/v1"
	// The commandcode gateway is the user's own OpenAI-compatible endpoint; it is
	// discovered from the local opencode configuration, like the OpenRouter key.
	CommandCodeBaseURL = "https://api.commandcode.ai/provider/v1"
	// Command Code exposes TypeSafe's decision model as System One, free for the
	// period documented in its pricing page.
	CommandCodeSystemOne = "https://api.commandcode.ai/provider/v1/systemone"
	CommandCodeJevModel  = "typesafe/jev"
	// DiffusionGemma serves the same System One wire format, needs no API key
	// and has a vision tower, so the calibrated tier can read screenshots.
	DiffusionGemmaEndpoint = "https://6ab255d535c41fcea4a331db.endpoints.huggingface.cloud/v1/systemone"
	DiffusionGemmaModel    = "jev-latest"
	DefaultCommandModel    = "deepseek/deepseek-v4.1-flash"
	DefaultCommandVision   = "deepseek/deepseek-v4-flash-vision-exp"
	opencodeConfigFile     = ".config/opencode/opencode.jsonc"
	DefaultLocalModel      = "local-decider"
	DefaultLocalVision     = "local-vision"
	opencodeAuthFile       = ".local/share/opencode/auth.json"
	cacheDirPermissions    = 0o755
	outputDirPermissions   = 0o755
)

// Limits bounds a run so it can never wander or spend without bound.
type Limits struct {
	MaxSteps        int           `json:"max_steps"`
	MaxDecisions    int           `json:"max_decisions"`
	MaxVisionCalls  int           `json:"max_vision_calls"`
	DecisionTimeout time.Duration `json:"decision_timeout"`
	VisionTimeout   time.Duration `json:"vision_timeout"`
}

// Thresholds are code-owned decision boundaries. Models return calibration;
// code branches on it.
type Thresholds struct {
	DefectProb      float64 `json:"defect_prob"`
	VerifyLow       float64 `json:"verify_low"`
	VerifyHigh      float64 `json:"verify_high"`
	SeverityFloor   float64 `json:"severity_floor"`
	PolicyProb      float64 `json:"policy_prob"`
	IdleChurn       float64 `json:"idle_churn"`
	FlickerCycles   int     `json:"flicker_cycles"`
	TemporalRepeats int     `json:"temporal_repeats"`
}

// Config is the whole runtime configuration.
type Config struct {
	Provider string `json:"provider"`

	JevModel    string `json:"jev_model"`
	JevEndpoint string `json:"jev_endpoint"`
	OpenRouter  string `json:"-"`

	VisionModel   string `json:"vision_model"`
	VisionBaseURL string `json:"vision_base_url"`
	VisionKey     string `json:"-"`

	LocalBaseURL     string `json:"local_base_url"`
	LocalModel       string `json:"local_model"`
	LocalVisionModel string `json:"local_vision_model"`

	// Compat is any OpenAI-compatible chat endpoint used as a decision tier
	// (the commandcode gateway, or llama.cpp with a chat model).
	CompatBaseURL string `json:"compat_base_url"`
	CompatModel   string `json:"compat_model"`
	CompatKey     string `json:"-"`

	Limits     Limits     `json:"limits"`
	Thresholds Thresholds `json:"thresholds"`

	CacheEnabled bool   `json:"cache_enabled"`
	CacheDir     string `json:"cache_dir"`
	RunDir       string `json:"run_dir"`
	// Triage is where reviewer verdicts live and where the calibration derived
	// from them is read from. Both are data, not code: replacing or deleting the
	// file changes the system's behaviour without a rebuild.
	LabelPath       string `json:"label_path"`
	CalibrationPath string `json:"calibration_path"`

	// SubjectOffline runs the subject program under a network-deny sandbox.
	SubjectOffline bool `json:"subject_offline"`
}

// Default returns the development-mode configuration.
func Default() Config {
	return Config{
		Provider:         "openrouter",
		JevModel:         DefaultJevModel,
		JevEndpoint:      OpenRouterAPI + "/alpha/decisions",
		VisionModel:      DefaultVisionModel,
		VisionBaseURL:    OpenRouterAPI + "/v1",
		LocalBaseURL:     DefaultLocalBaseURL,
		LocalModel:       DefaultLocalModel,
		LocalVisionModel: DefaultLocalVision,
		CompatBaseURL:    CommandCodeBaseURL,
		CompatModel:      DefaultCommandModel,
		Limits: Limits{
			MaxSteps:        40,
			MaxDecisions:    200,
			MaxVisionCalls:  12,
			DecisionTimeout: 30 * time.Second,
			VisionTimeout:   90 * time.Second,
		},
		Thresholds: Thresholds{
			DefectProb:      0.55,
			VerifyLow:       0.35,
			VerifyHigh:      0.80,
			SeverityFloor:   0.75,
			PolicyProb:      0.80,
			IdleChurn:       0.05,
			FlickerCycles:   2,
			TemporalRepeats: 3,
		},
		CacheEnabled:    true,
		CacheDir:        filepath.Join("var", "cache"),
		RunDir:          filepath.Join("var", "runs"),
		LabelPath:       filepath.Join("var", "triage", "labels.jsonl"),
		CalibrationPath: filepath.Join("var", "triage", "calibration.json"),
		SubjectOffline:  true,
	}
}

// Load applies environment overrides and discovers credentials.
func (c *Config) Load() error {
	overrideString(&c.Provider, "ARGUS_PROVIDER")
	overrideString(&c.JevModel, "ARGUS_JEV_MODEL")
	overrideString(&c.JevEndpoint, "ARGUS_JEV_ENDPOINT")
	overrideString(&c.VisionModel, "ARGUS_VISION_MODEL")
	overrideString(&c.VisionBaseURL, "ARGUS_VISION_BASE_URL")
	overrideString(&c.LocalBaseURL, "ARGUS_LOCAL_BASE_URL")
	overrideString(&c.LocalModel, "ARGUS_LOCAL_MODEL")
	overrideString(&c.LocalVisionModel, "ARGUS_LOCAL_VISION_MODEL")
	overrideString(&c.CompatBaseURL, "ARGUS_COMPAT_BASE_URL")
	overrideString(&c.CompatModel, "ARGUS_COMPAT_MODEL")
	overrideString(&c.CacheDir, "ARGUS_CACHE_DIR")
	overrideString(&c.RunDir, "ARGUS_RUN_DIR")
	overrideString(&c.LabelPath, "ARGUS_LABEL_PATH")
	overrideString(&c.CalibrationPath, "ARGUS_CALIBRATION_PATH")

	c.Provider = strings.ToLower(strings.TrimSpace(c.Provider))
	if raw := os.Getenv("ARGUS_NO_CACHE"); isTruthy(raw) {
		c.CacheEnabled = false
	}
	if raw := os.Getenv("ARGUS_SUBJECT_ONLINE"); isTruthy(raw) {
		c.SubjectOffline = false
	}

	c.OpenRouter = loadOpenRouterKey()
	c.CompatKey = firstNonEmpty(
		os.Getenv("ARGUS_COMPAT_API_KEY"),
		loadOpenCodeProviderKey("commandcode"),
		os.Getenv("COMMANDCODE_API_KEY"),
	)
	c.VisionKey = firstNonEmpty(os.Getenv("ARGUS_VISION_API_KEY"), c.OpenRouter)

	c.ApplyProviderDefaults()

	for _, dir := range []string{c.CacheDir, c.RunDir} {
		if err := os.MkdirAll(dir, outputDirPermissions); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	return nil
}

// ApplyProviderDefaults points each tier at the endpoint its provider uses. It
// runs after Load and again whenever the provider is chosen on the command line.
func (c *Config) ApplyProviderDefaults() {
	switch c.Provider {
	case "systemone":
		// Any Jev/System One compatible endpoint. Defaults to the free, key-less
		// DiffusionGemma read endpoint; OVERRIDE with ARGUS_JEV_ENDPOINT.
		c.JevEndpoint = firstNonEmpty(os.Getenv("ARGUS_JEV_ENDPOINT"), DiffusionGemmaEndpoint)
		if os.Getenv("ARGUS_JEV_MODEL") == "" {
			c.JevModel = DiffusionGemmaModel
		}
		c.VisionBaseURL = c.CompatBaseURL
		if os.Getenv("ARGUS_VISION_MODEL") == "" {
			c.VisionModel = DefaultCommandVision
		}
		c.VisionKey = firstNonEmpty(os.Getenv("ARGUS_VISION_API_KEY"), c.CompatKey)
	case "jev":
		c.JevEndpoint = CommandCodeSystemOne
		if os.Getenv("ARGUS_JEV_MODEL") == "" {
			c.JevModel = CommandCodeJevModel
		}
		c.VisionBaseURL = c.CompatBaseURL
		if os.Getenv("ARGUS_VISION_MODEL") == "" {
			c.VisionModel = DefaultCommandVision
		}
		c.VisionKey = firstNonEmpty(os.Getenv("ARGUS_VISION_API_KEY"), c.CompatKey)
	case "midasai":
		// Decisions and vision through Midas's pkg/ai, on the commandcode gateway
		// by default.
		c.CompatBaseURL = firstNonEmpty(os.Getenv("ARGUS_COMPAT_BASE_URL"), CommandCodeBaseURL)
		c.JevEndpoint = c.CompatBaseURL
		if os.Getenv("ARGUS_VISION_MODEL") == "" {
			c.VisionModel = DefaultCommandVision
		}
		c.VisionBaseURL = c.CompatBaseURL
		c.VisionKey = firstNonEmpty(os.Getenv("ARGUS_VISION_API_KEY"), c.CompatKey)
	case "commandcode":
		c.JevEndpoint = c.CompatBaseURL
		if os.Getenv("ARGUS_VISION_MODEL") == "" {
			c.VisionModel = DefaultCommandVision
		}
		c.VisionBaseURL = c.CompatBaseURL
		c.VisionKey = firstNonEmpty(os.Getenv("ARGUS_VISION_API_KEY"), c.CompatKey)
	case "local", "opensource":
		if os.Getenv("ARGUS_JEV_ENDPOINT") == "" {
			c.JevEndpoint = c.LocalBaseURL
		}
		if os.Getenv("ARGUS_VISION_BASE_URL") == "" {
			c.VisionBaseURL = c.LocalBaseURL
		}
	}
}

// AssertOfflineReady fails closed when a run claims to be offline but points at
// anything other than loopback.
func (c Config) AssertOfflineReady() error {
	if c.Provider != "local" && c.Provider != "opensource" {
		return errors.New("offline evaluation requires ARGUS_PROVIDER=local or opensource")
	}
	endpoints := []string{c.JevEndpoint, c.LocalBaseURL, c.VisionBaseURL}
	for _, endpoint := range endpoints {
		if endpoint == "" {
			continue
		}
		host := endpointHost(endpoint)
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			return fmt.Errorf("offline evaluation refused: non-loopback endpoint %s", endpoint)
		}
	}
	return nil
}

func endpointHost(raw string) string {
	trimmed := raw
	if idx := strings.Index(trimmed, "//"); idx >= 0 {
		trimmed = trimmed[idx+2:]
	}
	if idx := strings.IndexByte(trimmed, '/'); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	if idx := strings.LastIndexByte(trimmed, ':'); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	return trimmed
}

func overrideString(dst *string, env string) {
	if value := strings.TrimSpace(os.Getenv(env)); value != "" {
		*dst = value
	}
}

// loadOpenCodeProviderKey reads an OpenAI-compatible provider key from the local
// opencode configuration, so Argus can reuse credentials the user already has
// without copying secrets into this project.
func loadOpenCodeProviderKey(provider string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, opencodeConfigFile))
	if err != nil {
		return ""
	}
	// The file is JSON with // comments; strip whole-line comments before parsing.
	cleaned := make([]string, 0, 256)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		cleaned = append(cleaned, line)
	}
	var parsed struct {
		Provider map[string]struct {
			Options struct {
				APIKey  string `json:"apiKey"`
				BaseURL string `json:"baseURL"`
			} `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(strings.Join(cleaned, "\n")), &parsed); err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Provider[provider].Options.APIKey)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func isTruthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// loadOpenRouterKey reuses the key the local opencode install already stores so
// no secret needs to live in this repository.
func loadOpenRouterKey() string {
	for _, name := range []string{"OPENROUTER_API_KEY", "OPENROUTER_KEY"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, opencodeAuthFile))
	if err != nil {
		return ""
	}
	var store map[string]struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal(raw, &store); err != nil {
		return ""
	}
	return strings.TrimSpace(store["openrouter"].Key)
}
