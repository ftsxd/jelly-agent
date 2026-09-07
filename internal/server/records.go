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
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/record"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
)

// handleToolResult returns a window of one delivery, addressed by its handle.
//
// The handle, not the call id. A call id is unique within one run, not within
// a session — two runs of the same conversation both number their first call
// c1 — so addressing by it returned an arbitrary one of them, and the task
// centre, which shows several runs of one task side by side, is exactly where
// that surfaced. The handle (e7) is assigned by the store and is unique per
// session by construction.
//
// The session is in the path and is the permission boundary: the lookup is
// scoped by it in SQL rather than fetched and then compared, so a handle from
// one conversation cannot reach another's payload. A record in another scope
// and a record that never existed both answer 404 — distinguishing them would
// tell a caller which sessions exist.
func (s *Server) handleToolResult(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	ref := r.PathValue("ref")
	if sessionID == "" || ref == "" {
		writeErr(w, http.StatusBadRequest, "缺少 session 或结果引用")
		return
	}

	if !s.sessionStillThere(w, sessionID) {
		return
	}
	store, err := s.engine().Records()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	chunk, err := store.ReadLabel(r.Context(),
		record.Scope{AppName: engine.AppName, UserID: engine.UserID, SessionID: sessionID},
		ref,
		queryInt(r, "offset", 0, 0, 1<<30),
		queryInt(r, "limit", record.DefaultWindow, 1, record.MaxWindow),
	)
	// Expired and missing are different answers and need different words.
	//
	// "Not found" tells the reader to look again — a wrong id, another
	// session, a call whose delivery never landed. "Expired" tells them the
	// bytes are gone and the only way to get them is to run the tool again.
	// Collapsing the two sent people hunting for something that no longer
	// exists; before this, expiry fell through to a 500 with a raw error.
	if errors.Is(err, record.ErrExpired) {
		writeErr(w, http.StatusGone, "结果已过期，请重新查询")
		return
	}
	if errors.Is(err, record.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "该引用没有可重读的完整结果")
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
		"call_id":     chunk.CallID,
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

// handleToolResultSearch searches one stored delivery.
//
// The console needs this for the same reason the model does: a result too
// large to display is not too large to search, and paging through megabytes to
// find one line is the behaviour both are meant to avoid. It is the same
// Store.Search the search_result tool uses, so the two cannot disagree about
// what a match is or how many there were.
func (s *Server) handleToolResultSearch(w http.ResponseWriter, r *http.Request) {
	sessionID, ref := r.PathValue("id"), r.PathValue("ref")
	pattern := r.URL.Query().Get("q")
	if strings.TrimSpace(pattern) == "" {
		writeErr(w, http.StatusBadRequest, "q 不能为空")
		return
	}
	if !s.sessionStillThere(w, sessionID) {
		return
	}
	store, err := s.engine().Records()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Addressed by the same handle the read path takes, so the two cannot
	// disagree about which delivery is being talked about. There used to be a
	// call-id-to-handle resolution step here, which is where the ambiguity
	// between two runs' identically numbered calls entered.
	res, err := store.Search(r.Context(), record.Scope{
		AppName: engine.AppName, UserID: engine.UserID, SessionID: sessionID,
	}, ref, pattern, record.SearchOpts{
		Limit:   queryInt(r, "limit", record.DefaultHits, 1, record.MaxHits),
		Context: queryInt(r, "context", 1, 0, 20),
	})
	if errors.Is(err, record.ErrExpired) {
		writeErr(w, http.StatusGone, "结果已过期，请重新查询")
		return
	}
	if errors.Is(err, record.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "该引用没有可重读的完整结果")
		return
	}
	if err != nil {
		// A pattern the caller wrote wrongly is theirs to fix, and they can
		// only fix it if told what was wrong with it.
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// sessionStillThere refuses to serve a deleted conversation's deliveries.
//
// Deleting a session now removes its stored results too, so this is the second
// lock on the same door. It is here because the first one is a delete that can
// half succeed — and because these endpoints are addressed by a handle, which
// someone may still be holding from before. A deleted session and a session
// that never existed answer the same 404: telling them apart would tell a
// caller which conversations used to be here.
func (s *Server) sessionStillThere(w http.ResponseWriter, id string) bool {
	ok, err := jellysession.Exists(s.engine().SessionDBPath(), engine.AppName, engine.UserID, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return false
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return false
	}
	return true
}
