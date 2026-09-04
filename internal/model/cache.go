package model

// What the provider said about prompt caching.
//
// Caching is the single biggest lever on what a long conversation costs: a
// cached prompt token is billed at a fraction of a fresh one, and this agent
// re-sends the whole history on every round. So "is the cache working" is not
// a curiosity — it is the difference between a turn costing what it should and
// costing ten times that, and until it is measured every claim about it is a
// guess.
//
// The one thing this must not do is report a missing figure as zero. A
// provider that says nothing about caching and a provider that says "nothing
// was cached" look identical once both are an int, and the two call for
// opposite responses: the first means "we cannot see the cache", the second
// means "the cache is not working". Conflating them would let a broken cache
// hide behind a provider that simply does not report, which is exactly the
// kind of silent zero that made the 58k-token session unexplainable.
//
// Which field: probed against the configured provider (DeepSeek, 2026-09-04)
// with an identical 4595-token prefix sent twice. It returns both the
// OpenAI-standard prompt_tokens_details.cached_tokens and its own top-level
// prompt_cache_hit_tokens / prompt_cache_miss_tokens, always in agreement
// (4480 cached on the second call, 0 on the first), and streaming carries the
// same shape in the final chunk — so there is one code path, not two.
//
// Only the standard field is read. go-openai has no field for the DeepSeek
// pair, so it is dropped at decode; since DeepSeek sends both, reading the
// standard one loses nothing today. A provider that sent only a vendor field
// would be reported as unknown rather than as zero, which is the honest
// answer and not a wrong one.

import (
	openai "github.com/sashabaranov/go-openai"
)

// cacheUsage is one call's cache accounting.
type cacheUsage struct {
	// Prompt is the whole prompt, cached part included — providers report the
	// cached count as a subset of prompt_tokens, not in addition to it.
	Prompt int64
	// Cached is how much of it was served from cache.
	Cached int64
	// Known says whether Cached came from the provider at all. False means it
	// reported nothing; it must never be rendered as a zero hit rate.
	Known bool
}

// readCacheUsage extracts the cache accounting from an OpenAI usage block.
//
// PromptTokensDetails is a pointer in the SDK, which is what preserves the
// distinction: nil means the provider omitted the object, while a present
// object with CachedTokens zero is a real, reportable zero.
func readCacheUsage(u *openai.Usage) cacheUsage {
	if u == nil {
		return cacheUsage{}
	}
	c := cacheUsage{Prompt: int64(u.PromptTokens)}
	if d := u.PromptTokensDetails; d != nil {
		c.Cached, c.Known = int64(d.CachedTokens), true
	}
	return c
}
