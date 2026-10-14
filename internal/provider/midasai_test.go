//go:build midas

package provider

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/jev"
)

// These tests exercise the Midas pkg/ai tier against a live gateway. They skip
// unless a key is supplied, so the default suite stays offline.
func TestMidasAIChatTransport(t *testing.T) {
	key := os.Getenv("ARGUS_DEBUG_CC_KEY")
	if key == "" {
		t.Skip("set ARGUS_DEBUG_CC_KEY")
	}
	cfg := config.Default()
	cfg.Provider = "midasai"
	cfg.CompatKey = key
	cfg.CompatBaseURL = config.CommandCodeBaseURL
	cfg.CompatModel = "deepseek/deepseek-v4.1-flash"
	transport, err := NewMidasChatTransport(cfg)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	started := time.Now()
	text, err := transport.Chat(ctx, "Reply with JSON only.", `Reply exactly {"answers": {"ok": 1}}`, 0)
	t.Logf("after %s err=%v text=%q", time.Since(started).Round(time.Millisecond), err, truncate(text, 200))
}

func TestMidasAIDecisionsThroughTransport(t *testing.T) {
	key := os.Getenv("ARGUS_DEBUG_CC_KEY")
	if key == "" {
		t.Skip("set ARGUS_DEBUG_CC_KEY")
	}
	cfg := config.Default()
	cfg.Provider = "midasai"
	cfg.CompatKey = key
	cfg.CompatBaseURL = config.CommandCodeBaseURL
	cfg.CompatModel = "deepseek/deepseek-v4.1-flash"
	transport, err := NewMidasChatTransport(cfg)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	adapter := NewCompatDecisionsWith(transport, cfg.CompatModel)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	questions := map[string]jev.Question{
		"visible_defect": jev.Noul("Does `screen.text` show any defect a user would notice?"),
		"defect_class": jev.Choice("If a defect is visible, which class fits it best?",
			map[string]string{"none": "no defect", "duplication": "the same line twice", "truncation": "clipped text"}),
		"severity": jev.Score("How much does it affect a user?", []string{"cosmetic", "minor", "noticeable", "major", "critical"}),
	}
	state := map[string]any{"screen": map[string]any{"text": "1| Loading configuration...\n2| Loading configuration...\n3| +----+\n"}}
	result, err := adapter.Decide(ctx, state, questions, 1, nil)
	t.Logf("err=%v answers=%d", err, len(result.Answers))
	for id, answer := range result.Answers {
		t.Logf("  %s = %v (%v)", id, answer.P(), answer.Choice)
	}
	for _, warning := range result.Warnings {
		t.Logf("  warning: %s", truncate(warning, 160))
	}
}

func TestMidasAIVision(t *testing.T) {
	key := os.Getenv("ARGUS_DEBUG_CC_KEY")
	if key == "" {
		t.Skip("set ARGUS_DEBUG_CC_KEY")
	}
	cfg := config.Default()
	cfg.Provider = "midasai"
	cfg.CompatKey = key
	cfg.CompatBaseURL = config.CommandCodeBaseURL
	cfg.VisionKey = key
	cfg.VisionBaseURL = config.CommandCodeBaseURL
	cfg.VisionModel = config.DefaultCommandVision
	vision, err := NewMidasVision(cfg)
	if err != nil {
		t.Fatalf("vision: %v", err)
	}
	// A tiny red PNG is enough to prove the multimodal path works.
	image := redPixelPNG()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	text, err := vision.Describe(ctx, [][]byte{image}, "What colour is this pixel? One word.", nil)
	t.Logf("vision name=%s err=%v text=%q", vision.Name(), err, truncate(text, 120))
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
}

// redPixelPNG builds a tiny red image for transport tests.
func redPixelPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 220, G: 30, B: 30, A: 255})
		}
	}
	var buffer bytes.Buffer
	_ = png.Encode(&buffer, img)
	return buffer.Bytes()
}
