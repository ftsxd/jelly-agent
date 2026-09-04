package model

import (
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"google.golang.org/genai"
)

// The shapes below are not invented: they are what the configured provider
// (DeepSeek, probed 2026-09-04 with an identical 4595-token prefix sent twice)
// actually returned, plus the case of a provider that reports nothing.
func TestReadCacheUsageSeparatesAbsentFromZero(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage *openai.Usage
		want  cacheUsage
	}{{
		name: "第一次调用，提供方报告 0 命中",
		usage: &openai.Usage{
			PromptTokens: 4595, CompletionTokens: 4, TotalTokens: 4599,
			PromptTokensDetails: &openai.PromptTokensDetails{CachedTokens: 0},
		},
		want: cacheUsage{Prompt: 4595, Cached: 0, Known: true},
	}, {
		name: "第二次调用，命中 4480",
		usage: &openai.Usage{
			PromptTokens: 4595, CompletionTokens: 4, TotalTokens: 4599,
			PromptTokensDetails: &openai.PromptTokensDetails{CachedTokens: 4480},
		},
		want: cacheUsage{Prompt: 4595, Cached: 4480, Known: true},
	}, {
		// The case the whole design exists for: a provider that says nothing.
		// Reporting this as a zero hit would make an unobservable cache look
		// like a broken one.
		name: "提供方完全不报告缓存",
		usage: &openai.Usage{
			PromptTokens: 4595, CompletionTokens: 4, TotalTokens: 4599,
		},
		want: cacheUsage{Prompt: 4595, Cached: 0, Known: false},
	}, {
		name:  "没有 usage",
		usage: nil,
		want:  cacheUsage{},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := readCacheUsage(tc.usage)
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A zero hit and an unreported one must not produce the same usage metadata,
// because everything downstream — the console, the frames — reads that struct
// and cannot re-derive the difference.
func TestUsageMetadataKeepsCacheReportingDistinguishable(t *testing.T) {
	reportedZero := toUsage(&openai.Usage{
		PromptTokens: 100, TotalTokens: 104,
		PromptTokensDetails: &openai.PromptTokensDetails{CachedTokens: 0},
	})
	unreported := toUsage(&openai.Usage{PromptTokens: 100, TotalTokens: 104})

	if reportedZero.CacheTokensDetails == nil {
		t.Error("a reported zero lost its reporting marker; it is indistinguishable from silence")
	}
	if unreported.CacheTokensDetails != nil {
		t.Error("an unreported cache was marked as reported")
	}
	if reportedZero.CachedContentTokenCount != 0 || unreported.CachedContentTokenCount != 0 {
		t.Error("the counts should both be zero — the marker is what carries the difference")
	}

	hit := toUsage(&openai.Usage{
		PromptTokens: 4595, TotalTokens: 4599,
		PromptTokensDetails: &openai.PromptTokensDetails{CachedTokens: 4480},
	})
	if hit.CachedContentTokenCount != 4480 {
		t.Errorf("cached = %d, want 4480", hit.CachedContentTokenCount)
	}
	if len(hit.CacheTokensDetails) != 1 ||
		hit.CacheTokensDetails[0].TokenCount != 4480 ||
		hit.CacheTokensDetails[0].Modality != genai.MediaModalityText {
		t.Errorf("cache details = %+v, want one text entry of 4480", hit.CacheTokensDetails)
	}
	// The prompt count still includes the cached part — providers report the
	// cached figure as a subset, and treating it as additional would inflate
	// every total.
	if hit.PromptTokenCount != 4595 {
		t.Errorf("prompt = %d, want 4595 (cached tokens are part of it, not extra)", hit.PromptTokenCount)
	}
}
