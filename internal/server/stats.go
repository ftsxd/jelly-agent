package server

import (
	"net/http"
	"sort"

	adksession "google.golang.org/adk/session"

	"github.com/jelly-agent/jelly-agent/internal/engine"
)

// statsResponse is the aggregate the Monitor view renders.
type statsResponse struct {
	Sessions  int            `json:"sessions"`  // persisted session count
	Messages  int            `json:"messages"`  // text turns (user + agent)
	ToolCalls int            `json:"tool_calls"` // total tool invocations
	Tokens    tokenTotals    `json:"tokens"`
	Tools     []toolStat     `json:"tools"`     // per-tool invocation counts, desc
	Daily     []dailyStat    `json:"daily"`     // token/message series, chronological
	Providers providerStat   `json:"providers"`
	Memory    memoryStat     `json:"memory"`
}

type tokenTotals struct {
	Prompt     int32 `json:"prompt"`
	Completion int32 `json:"completion"`
	Total      int32 `json:"total"`
}

type toolStat struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type dailyStat struct {
	Date     string `json:"date"` // YYYY-MM-DD (local)
	Tokens   int32  `json:"tokens"`
	Messages int    `json:"messages"`
	Sessions int    `json:"sessions"`
}

type providerStat struct {
	Default string `json:"default"`
	Count   int    `json:"count"`
}

type memoryStat struct {
	SearchEnabled bool `json:"search_enabled"`
}

// handleStats aggregates usage across all persisted sessions for the Monitor
// view: token totals, per-tool invocation counts, and a per-day series. It
// reads each session in full (via Get) so UsageMetadata and tool calls are
// available; the local store is single-user and small, so the cost is modest.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	eng := s.engine()
	svc, err := eng.NewSessionService()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx := r.Context()
	list, err := svc.List(ctx, &adksession.ListRequest{AppName: engine.AppName, UserID: engine.UserID})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := statsResponse{
		Sessions: len(list.Sessions),
		Tools:    []toolStat{},
		Daily:    []dailyStat{},
		Providers: providerStat{
			Default: eng.Config().DefaultProvider,
			Count:   len(eng.Config().Providers),
		},
		Memory: memoryStat{SearchEnabled: eng.SearchEnabled()},
	}

	toolCounts := map[string]int{}
	type dayAgg struct {
		tokens   int32
		messages int
		sessions map[string]struct{}
	}
	days := map[string]*dayAgg{}

	for _, meta := range list.Sessions {
		resp, err := svc.Get(ctx, &adksession.GetRequest{AppName: engine.AppName, UserID: engine.UserID, SessionID: meta.ID()})
		if err != nil || resp.Session == nil {
			continue
		}
		sid := resp.Session.ID()
		for ev := range resp.Session.Events().All() {
			day := ev.Timestamp.Local().Format("2006-01-02")
			d := days[day]
			if d == nil {
				d = &dayAgg{sessions: map[string]struct{}{}}
				days[day] = d
			}
			d.sessions[sid] = struct{}{}

			if ev.UsageMetadata != nil {
				out.Tokens.Prompt += ev.UsageMetadata.PromptTokenCount
				out.Tokens.Completion += ev.UsageMetadata.CandidatesTokenCount
				out.Tokens.Total += ev.UsageMetadata.TotalTokenCount
				d.tokens += ev.UsageMetadata.TotalTokenCount
			}
			if ev.Content == nil {
				continue
			}
			hasText := false
			for _, p := range ev.Content.Parts {
				switch {
				case p == nil || p.Thought:
					continue
				case p.Text != "":
					hasText = true
				case p.FunctionCall != nil:
					toolCounts[p.FunctionCall.Name]++
					out.ToolCalls++
				}
			}
			if hasText {
				out.Messages++
				d.messages++
			}
		}
	}

	for name, n := range toolCounts {
		out.Tools = append(out.Tools, toolStat{Name: name, Count: n})
	}
	sort.Slice(out.Tools, func(i, j int) bool {
		if out.Tools[i].Count != out.Tools[j].Count {
			return out.Tools[i].Count > out.Tools[j].Count
		}
		return out.Tools[i].Name < out.Tools[j].Name
	})

	dates := make([]string, 0, len(days))
	for d := range days {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	for _, d := range dates {
		agg := days[d]
		out.Daily = append(out.Daily, dailyStat{
			Date:     d,
			Tokens:   agg.tokens,
			Messages: agg.messages,
			Sessions: len(agg.sessions),
		})
	}

	writeJSON(w, http.StatusOK, out)
}
