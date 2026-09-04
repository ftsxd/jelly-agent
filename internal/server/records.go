package server

// Reading back what a tool delivered.
//
// The point of storing a full delivery is to answer a question after the fact:
// the prompt only ever saw a shortened version, and someone reviewing the run
// needs the part that was cut. That is what this endpoint is for.
//
// Two things it deliberately does not do. It does not hand back the whole
// payload: a result that was too large for a prompt is still too large for one
// response, so a caller asks for a window and pages. And it does not promise
// that the payload is the tool's complete output — a tool may have truncated
// its own result before the gateway ever saw it (fetch_url does, inside the
// tool), so the answer carries what the tool said about that, including
// "unknown" when it said nothing.

import (
	"errors"
	"net/http"

	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/record"
)

// handleToolResult returns a window of one call's delivered result.
//
// The session is in the path and is the permission boundary: the lookup is
// scoped by it in SQL rather than fetched and then compared, so a call id from
// one conversation cannot reach another's payload. A record in another scope
// and a record that never existed both answer 404 — distinguishing them would
// tell a caller which sessions exist.
func (s *Server) handleToolResult(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	callID := r.PathValue("call")
	if sessionID == "" || callID == "" {
		writeErr(w, http.StatusBadRequest, "缺少 session 或 call id")
		return
	}

	store, err := s.engine().Records()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	chunk, err := store.Read(r.Context(),
		record.Scope{AppName: engine.AppName, UserID: engine.UserID, SessionID: sessionID},
		callID,
		queryInt(r, "offset", 0, 0, 1<<30),
		queryInt(r, "limit", record.DefaultWindow, 1, record.MaxWindow),
	)
	if errors.Is(err, record.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "该调用没有可重读的完整结果")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	next := chunk.Offset + len(chunk.Data)
	writeJSON(w, http.StatusOK, map[string]any{
		"tool":        chunk.Tool,
		"server":      chunk.Server,
		"at":          chunk.At,
		"label":       chunk.Label,
		"total":       chunk.Total,
		"offset":      chunk.Offset,
		"data":        string(chunk.Data),
		"sha256":      chunk.SHA256,
		"has_more":    next < chunk.Total,
		"next_offset": next,
		// What the tool itself said about truncating. "unknown" is the honest
		// answer for a third-party server that reports nothing, and must not
		// be read as "complete".
		"upstream_truncated": string(chunk.Upstream),
	})
}
