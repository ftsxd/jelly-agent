package server

import (
	"log/slog"
	"net/http"
	"sort"
	"time"

	adksession "google.golang.org/adk/session"

	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/metrics"

	"github.com/jelly-agent/jelly-agent/internal/logging"
)

// statsResponse is the aggregate the Monitor view renders.
type statsResponse struct {
	Sessions  int `json:"sessions"`   // persisted session count
	Messages  int `json:"messages"`   // text turns (user + agent)
	ToolCalls int `json:"tool_calls"` // total tool invocations
	// ToolResults / ToolErrors count responses, not calls: a call whose response
	// never came back (the run was cancelled mid-tool) is in neither. Success
	// rate is therefore (ToolResults-ToolErrors)/ToolResults, not …/ToolCalls.
	ToolResults int           `json:"tool_results"`
	ToolErrors  int           `json:"tool_errors"`
	Tokens      tokenTotals   `json:"tokens"`
	Tools       []toolStat    `json:"tools"` // per-tool invocation counts, desc
	Daily       []dailyStat   `json:"daily"` // token/message series, chronological
	Providers   providerStat  `json:"providers"`
	Memory      memoryStat    `json:"memory"`
	Telemetry   telemetryStat `json:"telemetry"`
}

type tokenTotals struct {
	Prompt     int32 `json:"prompt"`
	Completion int32 `json:"completion"`
	Total      int32 `json:"total"`
}

// toolStat mixes two sources on purpose, and the split matters when reading it.
//
// Count/Results/Failed are scanned out of session events, so they cover every
// call ever made. Timed/P50MS/P95MS/MaxMS/ErrKinds come from the tool_calls
// table, which only has rows from the moment the telemetry hooks were added —
// per-call duration was never written to events and cannot be recovered from
// them. Timed is therefore usually smaller than Count, and a UI that shows both
// must say so or the two look like a contradiction.
type toolStat struct {
	Name    string `json:"name"`
	Count   int    `json:"count"`
	Results int    `json:"results"`
	Failed  int    `json:"failed"`

	Timed    int            `json:"timed"` // recorded calls backing the fields below
	OK       int            `json:"ok"`
	P50MS    int            `json:"p50_ms,omitempty"`
	P95MS    int            `json:"p95_ms,omitempty"`
	MaxMS    int            `json:"max_ms,omitempty"`
	ErrKinds map[string]int `json:"err_kinds,omitempty"` // cause bucket -> count
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

// telemetryStat tells the UI how much of the timing picture exists, so it can
// distinguish "this tool is fast" from "we have not measured this tool yet".
type telemetryStat struct {
	Calls int    `json:"calls"`
	Since string `json:"since,omitempty"` // RFC3339 of the oldest recorded call
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
	toolResults := map[string]int{}
	toolFails := map[string]int{}
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
				case p.FunctionResponse != nil:
					name := p.FunctionResponse.Name
					toolResults[name]++
					out.ToolResults++
					if toolFailed(p.FunctionResponse.Response) {
						toolFails[name]++
						out.ToolErrors++
					}
				}
			}
			if hasText {
				out.Messages++
				d.messages++
			}
		}
	}

	for name := range toolResults {
		if _, seen := toolCounts[name]; !seen {
			toolCounts[name] = 0 // a response whose call event is missing
		}
	}
	// Fold in recorded timing. A tool that has rows but no events (or the
	// reverse) still gets a line: hiding either half would misreport the very
	// gap this merge exists to expose.
	timing := map[string]metrics.ToolLatency{}
	if sum, err := eng.Metrics().Summary(time.Time{}); err != nil {
		slog.Warn("工具耗时数据不可用", logging.Err(err))
	} else {
		out.Telemetry.Calls = sum.Calls
		if !sum.Since.IsZero() {
			out.Telemetry.Since = sum.Since.Format(time.RFC3339)
		}
		for _, t := range sum.Tools {
			timing[t.Tool] = t
			if _, seen := toolCounts[t.Tool]; !seen {
				toolCounts[t.Tool] = 0
			}
		}
	}

	for name, n := range toolCounts {
		st := toolStat{Name: name, Count: n, Results: toolResults[name], Failed: toolFails[name]}
		if t, ok := timing[name]; ok {
			st.Timed, st.OK = t.Calls, t.OK
			st.P50MS, st.P95MS, st.MaxMS = t.P50MS, t.P95MS, t.MaxMS
			st.ErrKinds = t.ErrKinds
		}
		out.Tools = append(out.Tools, st)
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
