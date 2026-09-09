// Package server exposes jelly-agent's runtime over HTTP for the web dashboard:
// a small REST surface (providers, tools, sessions, memory) plus an SSE chat
// stream. It drives the same internal/engine the CLI uses, so both front ends
// share one agent, one session store, and one memory layer.
package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/memory"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"

	"github.com/jelly-agent/jelly-agent/internal/storage"

	"github.com/jelly-agent/jelly-agent/internal/logging"
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

	bots     botManager // running messaging-platform bots (DingTalk, …)
	auth     *authManager
	schedule scheduler

	// runReg knows which runs are in flight, which is the one thing about a
	// task that cannot be read off its stored events. See runs.go.
	runsOnce sync.Once
	// declMu serialises writes to the console's tool-metadata file. Every save
	// is a read-modify-write of one file, and the page saves a field at a time.
	declMu sync.Mutex

	// recordsProbe answers which runs of a set of sessions stored anything.
	//
	// A field rather than a direct call so a test can count how often the task
	// list asks. "Once per page of sessions" is a property of the call site,
	// not of the query — a test of the query alone stays green the day
	// somebody puts it back inside the per-session loop, which is exactly the
	// regression this is guarding. Nil means the delivery store answers it.
	recordsProbe func(ctx context.Context, appName, userID string, sessionIDs []string) (map[string]map[string]bool, error)
	runReg       *runRegistry
}

// New builds a server over the given engine. staticFS is the embedded frontend
// build rooted at the dir holding index.html (pass nil to serve API only, e.g.
// during frontend dev with Vite's proxy).
func New(eng *engine.Engine, staticFS fs.FS) *Server {
	// One-time cleanup of event rows orphaned by deletes that ran before sessions
	// and events were removed together (SQLite foreign keys are off, so ADK's
	// cascade never fired). Best-effort: a fresh/empty DB simply has none.
	// Resolved from the engine rather than defaulted, for the same reason the
	// delete path is: a deployment with its own database must not have its
	// housekeeping run against a different one.
	if db, err := eng.StateDB(); err != nil {
		// Best-effort, and the only place that says so out loud: a database
		// that will not open is reported by every handler that needs it, so
		// failing construction here would trade a degraded console for none.
		slog.Warn("状态数据库打不开，跳过启动清理", logging.Err(err))
	} else {
		if n, err := jellysession.PurgeOrphanEvents(db); err == nil && n > 0 {
			slog.Info("清理孤儿会话事件", "rows", n)
		}
		// Same for the L2 search index: drop rows whose session was deleted
		// before the index was purged alongside it, so load_memory can't
		// surface them.
		if n, err := memory.PurgeOrphanIndex(db); err == nil && n > 0 {
			slog.Info("清理孤儿检索索引", "rows", n)
		}
	}
	s := &Server{eng: eng, static: staticFS, auth: newAuthManager()}
	s.attachScheduleTools(eng)
	return s
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

// stateDB is the shared handle on the state database, or an error a handler
// can answer with.
//
// A helper rather than each handler repeating it, because the failure is the
// same everywhere: the database could not be opened, which is a 500 and not
// something a request can recover from. The handle itself is opened once for
// the process — the stores that live in this database used to open one per
// call, which costs nothing against a local file and a full connection
// handshake against PostgreSQL.
func (s *Server) stateDB(w http.ResponseWriter) (*storage.DB, bool) {
	db, err := s.engine().StateDB()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "打不开状态数据库: "+err.Error())
		return nil, false
	}
	return db, true
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
	s.attachScheduleTools(s.eng)
	s.mu.Unlock()
	if old != nil {
		old.Close() // terminate the previous engine's stdio MCP subprocesses
	}
	s.restartBots(cfg) // pick up platform changes; bots answer via the new engine
	s.restartSchedules()
	return nil
}

// Handler returns the root HTTP handler: /api/* routes plus the SPA fallback.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("POST /api/auth/password", s.handleChangePassword)
	mux.HandleFunc("GET /api/schedules", s.handleSchedules)
	mux.HandleFunc("POST /api/schedules", s.handleSaveSchedule)
	mux.HandleFunc("DELETE /api/schedules/{name}", s.handleDeleteSchedule)
	mux.HandleFunc("POST /api/schedules/{name}/run", s.handleRunSchedule)
	mux.HandleFunc("GET /api/schedules/runs", s.handleScheduleRuns)
	mux.HandleFunc("GET /api/providers", s.handleProviders)
	mux.HandleFunc("POST /api/providers", s.handleSaveProvider)
	mux.HandleFunc("DELETE /api/providers/{name}", s.handleDeleteProvider)
	mux.HandleFunc("GET /api/tools", s.handleTools)
	mux.HandleFunc("POST /api/tools/test", s.handleToolTest)
	mux.HandleFunc("POST /api/tools/fetch", s.handleToolFetch)
	mux.HandleFunc("GET /api/tools/metadata", s.handleToolMetadata)
	mux.HandleFunc("POST /api/tools/metadata", s.handleSaveToolMetadata)
	mux.HandleFunc("GET /api/mcp", s.handleListMCP)
	mux.HandleFunc("POST /api/mcp", s.handleSaveMCP)
	mux.HandleFunc("POST /api/mcp/test", s.handleTestMCP)
	mux.HandleFunc("DELETE /api/mcp/{name}", s.handleDeleteMCP)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("GET /api/sessions/ids", s.handleSessionIDs)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleSessionDetail)
	mux.HandleFunc("GET /api/sessions/{id}/timeline", s.handleSessionTimeline)
	mux.HandleFunc("GET /api/sessions/{id}/results/{ref}", s.handleToolResult)
	mux.HandleFunc("GET /api/sessions/{id}/results/{ref}/search", s.handleToolResultSearch)
	mux.HandleFunc("GET /api/tasks", s.handleTasks)
	mux.HandleFunc("GET /api/tasks/{session}/{round}", s.handleTask)
	mux.HandleFunc("POST /api/sessions/delete", s.handleDeleteSessions)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("GET /api/skills", s.handleListSkills)
	mux.HandleFunc("GET /api/skills/{name}", s.handleSkillDetail)
	mux.HandleFunc("POST /api/skills", s.handleSaveSkill)
	mux.HandleFunc("POST /api/skills/upload", s.handleUploadSkill)
	mux.HandleFunc("POST /api/skills/allow-scripts", s.handleSetAllowScripts)
	mux.HandleFunc("GET /api/sandbox", s.handleSandbox)
	mux.HandleFunc("POST /api/sandbox", s.handleSetSandbox)
	mux.HandleFunc("GET /api/agents", s.handleListAgents)
	mux.HandleFunc("POST /api/agents", s.handleSaveAgent)
	mux.HandleFunc("DELETE /api/agents/{name}", s.handleDeleteAgent)
	mux.HandleFunc("POST /api/skills/{name}/vars", s.handleSetSkillVars)
	mux.HandleFunc("DELETE /api/skills/{name}/vars/{key}", s.handleDeleteSkillVar)
	mux.HandleFunc("DELETE /api/skills/{name}", s.handleDeleteSkill)
	mux.HandleFunc("GET /api/memory/core", s.handleMemoryCore)
	mux.HandleFunc("POST /api/memory/core", s.handleSetMemoryCore)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("PUT /api/history", s.handleSetHistory)
	mux.HandleFunc("GET /api/memory/search", s.handleMemorySearch)
	mux.HandleFunc("PUT /api/memory/search", s.handleSetMemorySearch)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/prompt", s.handlePrompt)
	mux.HandleFunc("PUT /api/prompt/instruction", s.handleSaveInstruction)
	mux.HandleFunc("GET /api/platforms", s.handleListPlatforms)
	mux.HandleFunc("POST /api/platforms", s.handleSavePlatform)
	mux.HandleFunc("DELETE /api/platforms/{name}", s.handleDeletePlatform)
	mux.HandleFunc("POST /api/chat/stream", s.handleChatStream)

	if s.static != nil {
		mux.Handle("/", s.spaHandler())
	}
	return s.authMiddleware(mux)
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
