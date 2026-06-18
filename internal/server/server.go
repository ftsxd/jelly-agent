// Package server exposes jelly-agent's runtime over HTTP for the web dashboard:
// a small REST surface (providers, tools, sessions, memory) plus an SSE chat
// stream. It drives the same internal/engine the CLI uses, so both front ends
// share one agent, one session store, and one memory layer.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
)

// Server wires the engine and embedded frontend assets to an HTTP handler. The
// engine is swappable behind a mutex so config edits hot-reload without a
// restart (PLAN §1.5 "配置热重载，对话不中断").
type Server struct {
	mu         sync.RWMutex
	eng        *engine.Engine
	static     fs.FS  // embedded SPA build (dist); nil disables static serving
	configPath string // explicit --config path for reloads ("" = auto-resolve)

	pollInterval time.Duration // config-watch poll interval; 0 = defaultConfigPoll
	out          io.Writer     // hot-reload notices; nil = os.Stderr

	bots botManager // running messaging-platform bots (DingTalk, …)
}

// New builds a server over the given engine. staticFS is the embedded frontend
// build rooted at the dir holding index.html (pass nil to serve API only, e.g.
// during frontend dev with Vite's proxy).
func New(eng *engine.Engine, staticFS fs.FS) *Server {
	return &Server{eng: eng, static: staticFS}
}

// WithConfigPath records the explicit config path (from --config / $JELLY_CONFIG)
// so reloads after a web edit resolve the same file. Returns s for chaining.
func (s *Server) WithConfigPath(path string) *Server {
	s.configPath = path
	return s
}

// engine returns the current engine under a read lock.
func (s *Server) engine() *engine.Engine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.eng
}

// reload re-reads config from disk and swaps in a fresh engine, so subsequent
// requests (new chats build their agent per-request) use the updated providers.
func (s *Server) reload() error {
	cfg, err := config.LoadOrEnv(s.configPath)
	if err != nil {
		return err
	}
	s.mu.Lock()
	old := s.eng
	s.eng = engine.New(cfg)
	s.mu.Unlock()
	if old != nil {
		old.Close() // terminate the previous engine's stdio MCP subprocesses
	}
	s.restartBots(cfg) // pick up platform changes; bots answer via the new engine
	return nil
}

// Handler returns the root HTTP handler: /api/* routes plus the SPA fallback.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/providers", s.handleProviders)
	mux.HandleFunc("POST /api/providers", s.handleSaveProvider)
	mux.HandleFunc("DELETE /api/providers/{name}", s.handleDeleteProvider)
	mux.HandleFunc("GET /api/tools", s.handleTools)
	mux.HandleFunc("POST /api/tools/test", s.handleToolTest)
	mux.HandleFunc("GET /api/mcp", s.handleListMCP)
	mux.HandleFunc("POST /api/mcp", s.handleSaveMCP)
	mux.HandleFunc("POST /api/mcp/test", s.handleTestMCP)
	mux.HandleFunc("DELETE /api/mcp/{name}", s.handleDeleteMCP)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleSessionDetail)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("GET /api/skills", s.handleListSkills)
	mux.HandleFunc("GET /api/skills/{name}", s.handleSkillDetail)
	mux.HandleFunc("POST /api/skills", s.handleSaveSkill)
	mux.HandleFunc("POST /api/skills/upload", s.handleUploadSkill)
	mux.HandleFunc("DELETE /api/skills/{name}", s.handleDeleteSkill)
	mux.HandleFunc("GET /api/memory/core", s.handleMemoryCore)
	mux.HandleFunc("POST /api/memory/core", s.handleSetMemoryCore)
	mux.HandleFunc("GET /api/memory/search", s.handleMemorySearch)
	mux.HandleFunc("PUT /api/memory/search", s.handleSetMemorySearch)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/platforms", s.handleListPlatforms)
	mux.HandleFunc("POST /api/platforms", s.handleSavePlatform)
	mux.HandleFunc("DELETE /api/platforms/{name}", s.handleDeletePlatform)
	mux.HandleFunc("POST /api/chat/stream", s.handleChatStream)

	if s.static != nil {
		mux.Handle("/", s.spaHandler())
	}
	return mux
}

// writeJSON encodes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr sends a JSON error envelope: {"error": "..."}.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// logf writes an operational notice (e.g. config hot-reload events) to s.out,
// defaulting to stderr. Tests redirect it to a buffer or io.Discard.
func (s *Server) logf(format string, args ...any) {
	w := s.out
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "[jelly] "+format+"\n", args...)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"app":     engine.AppName,
		"user":    engine.UserID,
		"version": Version,
	})
}

// Version is the server version, overridable at build time via -ldflags.
var Version = "0.2.0-dev"
