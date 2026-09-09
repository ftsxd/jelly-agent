package model

import (
	"context"
	"os"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
	"gopkg.in/yaml.v3"
)

// Live probe: not part of the suite (skipped without JELLY_LIVE=1). It exists
// because every unit test above encodes a shape I observed once, and a shape
// can change under us.
func TestLiveCacheReportingReachesUsageMetadata(t *testing.T) {
	if os.Getenv("JELLY_LIVE") != "1" {
		t.Skip("set JELLY_LIVE=1 to call the real provider")
	}
	home, _ := os.UserHomeDir()
	raw, err := os.ReadFile(home + "/.jelly-agent/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Providers []struct {
			BaseURL string `yaml:"base_url"`
			APIKey  string `yaml:"api_key"`
			Model   string `yaml:"model"`
		} `yaml:"providers"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	p := cfg.Providers[0]

	llm := New(ProviderConfig{BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model})
	long := "只回答“收到”。\n"
	for i := 0; i < 300; i++ {
		long += "节点巡检项：磁盘、内存、负载、网络、进程存活。"
	}

	for _, stream := range []bool{false, true} {
		var last *adkmodel.LLMResponse
		req := &adkmodel.LLMRequest{
			Contents: []*genai.Content{genai.NewContentFromText(long, genai.RoleUser)},
		}
		for resp, err := range llm.GenerateContent(context.Background(), req, stream) {
			if err != nil {
				t.Fatalf("stream=%v: %v", stream, err)
			}
			if resp != nil && !resp.Partial {
				last = resp
			}
		}
		if last == nil || last.UsageMetadata == nil {
			t.Fatalf("stream=%v: no usage metadata", stream)
		}
		u := last.UsageMetadata
		t.Logf("stream=%v prompt=%d cached=%d reported=%v",
			stream, u.PromptTokenCount, u.CachedContentTokenCount, u.CacheTokensDetails != nil)
		if u.CacheTokensDetails == nil {
			t.Errorf("stream=%v: provider reported no cache figure; the metric would show no coverage", stream)
		}
	}
}
