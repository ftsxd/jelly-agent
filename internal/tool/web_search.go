// Package tool holds jelly-agent's built-in tools, registered with ADK via
// functiontool so the LLM can call them.
package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// WebSearchArgs are the parameters the model fills when calling web_search.
// The `jsonschema` tag becomes the property description in the tool schema
// sent to the model.
type WebSearchArgs struct {
	Query  string `json:"query" jsonschema:"搜索关键词"`
	MaxNum int    `json:"max_num,omitempty" jsonschema:"返回条数，默认 5"`
}

// SearchItem is a single search hit.
type SearchItem struct {
	Title   string `json:"title"`
	URL     string `json:"url,omitempty"`
	Snippet string `json:"snippet"`
}

// WebSearchResult is the structured tool output handed back to the model.
type WebSearchResult struct {
	Query   string       `json:"query"`
	Source  string       `json:"source"`
	Results []SearchItem `json:"results"`
}

const (
	defaultMaxResults = 5
	maxSearchResults  = 10
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// NewWebSearchTool builds the web_search tool. It uses Tavily when
// TAVILY_API_KEY is set (better results) and otherwise falls back to the
// key-free DuckDuckGo Instant Answer API, so it works with no extra config.
func NewWebSearchTool() (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name:        "web_search",
			Description: "搜索互联网获取实时信息。传入查询关键词，返回若干条标题/摘要/链接。",
		},
		func(tc adktool.Context, args WebSearchArgs) (WebSearchResult, error) {
			// tc is itself a context.Context (via ADK ReadonlyContext), so the
			// tool's HTTP request is cancelled when the upstream call / SSE
			// connection is cancelled.
			return Search(tc, args.Query, args.MaxNum)
		},
	)
}

// Search runs a web search with the same backend selection as the web_search
// tool (Tavily when TAVILY_API_KEY is set, else key-free DuckDuckGo). It is
// exported so the web server's tool-test endpoint can exercise the tool without
// going through the model. A non-positive max uses the default; an oversized max
// is clamped.
func Search(ctx context.Context, query string, max int) (WebSearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return WebSearchResult{}, fmt.Errorf("query 不能为空")
	}
	switch {
	case max <= 0:
		max = defaultMaxResults
	case max > maxSearchResults:
		max = maxSearchResults
	}
	if key := os.Getenv("TAVILY_API_KEY"); key != "" {
		return tavilySearch(ctx, key, query, max)
	}
	return duckDuckGoSearch(ctx, query, max)
}

// tavilySearch queries the Tavily search API.
func tavilySearch(ctx context.Context, apiKey, query string, max int) (WebSearchResult, error) {
	body, _ := json.Marshal(map[string]any{
		"api_key":     apiKey,
		"query":       query,
		"max_results": max,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", bytes.NewReader(body))
	if err != nil {
		return WebSearchResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return WebSearchResult{}, fmt.Errorf("tavily request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return WebSearchResult{}, fmt.Errorf("tavily status %d", resp.StatusCode)
	}

	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return WebSearchResult{}, fmt.Errorf("tavily decode: %w", err)
	}

	out := WebSearchResult{Query: query, Source: "tavily"}
	for _, r := range payload.Results {
		out.Results = append(out.Results, SearchItem{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}

// duckDuckGoSearch queries DuckDuckGo's key-free Instant Answer API.
func duckDuckGoSearch(ctx context.Context, query string, max int) (WebSearchResult, error) {
	endpoint := "https://api.duckduckgo.com/?" + url.Values{
		"q":             {query},
		"format":        {"json"},
		"no_html":       {"1"},
		"no_redirect":   {"1"},
		"skip_disambig": {"1"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return WebSearchResult{}, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return WebSearchResult{}, fmt.Errorf("duckduckgo request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// DuckDuckGo's Instant Answer API often returns 202 (rate limit / async).
		// Return a usable note instead of a hard error so the model can proceed.
		return WebSearchResult{
			Query:  query,
			Source: "duckduckgo",
			Results: []SearchItem{{
				Snippet: fmt.Sprintf("DuckDuckGo 未返回结果（HTTP %d，即时答案 API 覆盖有限或被限流）。配置 TAVILY_API_KEY 可获得完整网页搜索结果。", resp.StatusCode),
			}},
		}, nil
	}

	var payload struct {
		AbstractText  string `json:"AbstractText"`
		AbstractURL   string `json:"AbstractURL"`
		Heading       string `json:"Heading"`
		RelatedTopics []struct {
			Text     string `json:"Text"`
			FirstURL string `json:"FirstURL"`
		} `json:"RelatedTopics"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return WebSearchResult{}, fmt.Errorf("duckduckgo decode: %w", err)
	}

	out := WebSearchResult{Query: query, Source: "duckduckgo"}
	if payload.AbstractText != "" {
		out.Results = append(out.Results, SearchItem{
			Title:   payload.Heading,
			URL:     payload.AbstractURL,
			Snippet: payload.AbstractText,
		})
	}
	for _, t := range payload.RelatedTopics {
		if len(out.Results) >= max {
			break
		}
		if t.Text == "" {
			continue
		}
		out.Results = append(out.Results, SearchItem{Title: t.Text, URL: t.FirstURL, Snippet: t.Text})
	}
	if len(out.Results) == 0 {
		out.Results = append(out.Results, SearchItem{
			Snippet: "未找到即时答案。DuckDuckGo Instant Answer 仅覆盖部分查询；配置 TAVILY_API_KEY 可获得完整网页搜索结果。",
		})
	}
	return out, nil
}
