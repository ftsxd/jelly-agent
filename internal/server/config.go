package server

import (
	"net/http"
	"os"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/history"
)

// providerInput is the body for POST /api/providers (create or update).
//
// The four tuning fields are pointers so an absent JSON key means "leave as
// is": callers that only touch one thing (e.g. the list's "set as default"
// button, which posts just name/base_url/model) must not silently wipe them.
// An explicit value writes through, and an explicit 0 clears back to the
// endpoint default — except max_retries, where 0 legitimately means "never
// retry", so only omitting the key keeps the current setting.
type providerInput struct {
	Name        string `json:"name"`
	BaseURL     string `json:"base_url"`
	APIKey      string `json:"api_key"` // empty on update = keep existing key
	Model       string `json:"model"`
	MakeDefault bool   `json:"make_default"`

	Temperature   *float64 `json:"temperature"`
	MaxTokens     *int     `json:"max_tokens"`
	TimeoutSec    *int     `json:"timeout_sec"`
	MaxRetries    *int     `json:"max_retries"`
	ContextWindow *int     `json:"context_window"`
}

// applyTuning copies the supplied tuning fields onto p, leaving absent ones
// untouched. Out-of-range values are clamped rather than rejected so a stray
// negative from a client can't produce a nonsensical config.
func (in providerInput) applyTuning(p *config.Provider) {
	if in.Temperature != nil {
		// go-openai drops a 0 temperature (`omitempty`), so storing it would be
		// a lie; treat 0 as "unset" and keep the file clean.
		if t := *in.Temperature; t > 0 {
			p.Temperature = &t
		} else {
			p.Temperature = nil
		}
	}
	if in.MaxTokens != nil {
		p.MaxTokens = max(0, *in.MaxTokens)
	}
	if in.TimeoutSec != nil {
		p.TimeoutSec = max(0, *in.TimeoutSec)
	}
	if in.MaxRetries != nil {
		n := max(0, *in.MaxRetries)
		p.MaxRetries = &n
	}
	if in.ContextWindow != nil {
		p.ContextWindow = max(0, *in.ContextWindow)
	}
}

// handleSaveProvider upserts a provider into the config file and hot-reloads the
// engine. Keys are written verbatim; an empty api_key on an existing provider
// preserves the stored value (including ${ENV} references), so editing an
// unrelated field never bakes a secret into the file.
func (s *Server) handleSaveProvider(w http.ResponseWriter, r *http.Request) {
	var in providerInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	in.Model = strings.TrimSpace(in.Model)
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name 不能为空")
		return
	}

	path, err := s.writeTargetPath()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := loadRawOrEmpty(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	idx := indexOfProvider(raw.Providers, in.Name)
	if idx < 0 {
		// Create — base_url and model are required for the agent to work.
		if in.BaseURL == "" || in.Model == "" {
			writeErr(w, http.StatusBadRequest, "新建 Provider 需填写 base_url 与 model")
			return
		}
		created := config.Provider{
			Name: in.Name, BaseURL: in.BaseURL, APIKey: in.APIKey, Model: in.Model,
		}
		in.applyTuning(&created)
		raw.Providers = append(raw.Providers, created)
	} else {
		p := &raw.Providers[idx]
		p.BaseURL = in.BaseURL
		p.Model = in.Model
		if strings.TrimSpace(in.APIKey) != "" {
			p.APIKey = in.APIKey // only overwrite when a new key is supplied
		}
		in.applyTuning(p)
	}

	// First provider becomes default automatically; explicit request overrides.
	if in.MakeDefault || raw.DefaultProvider == "" {
		raw.DefaultProvider = in.Name
	}

	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_to": path})
}

// handleDeleteProvider removes a provider and hot-reloads.
func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	path, err := s.writeTargetPath()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := config.LoadRaw(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeErr(w, http.StatusNotFound, "尚无配置文件可删除")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	idx := indexOfProvider(raw.Providers, name)
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "provider 不存在")
		return
	}
	raw.Providers = append(raw.Providers[:idx], raw.Providers[idx+1:]...)
	if raw.DefaultProvider == name {
		raw.DefaultProvider = ""
		if len(raw.Providers) > 0 {
			raw.DefaultProvider = raw.Providers[0].Name
		}
	}

	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_to": path})
}

// historyInput configures conversation compaction from the web UI.
//
// Unlike providerInput there is no partial-update caller here — the form always
// posts the whole section — so plain "absent means default" semantics are
// enough. MaxTokens stays a pointer only to separate "use the default" (null)
// from "turn compaction off" (0).
type historyInput struct {
	MaxTokens        *int `json:"max_tokens"`
	KeepRecent       int  `json:"keep_recent"`
	ToolResultTokens int  `json:"tool_result_tokens"`
	// MaxResultBytes lives under `tools` in the config file, which is where it
	// belongs, but it is edited here on purpose: it and history compaction are
	// the only two things bounding what reaches the context window, and they
	// interact. Turning compaction off is safe only while this ceiling is set,
	// and leaving this at zero is safe only while compaction is on. A form
	// that showed one without the other would invite exactly the combination
	// that leaves neither.
	MaxResultBytes int `json:"max_result_bytes"`
}

// handleHistory reports the current compaction settings plus the defaults that
// apply when a field is unset, so the form can show what is actually in force.
func (s *Server) handleHistory(w http.ResponseWriter, _ *http.Request) {
	cfg := s.engine().Config()
	h := cfg.History
	writeJSON(w, http.StatusOK, map[string]any{
		"max_tokens":         h.MaxTokens, // null ⇒ default applies
		"keep_recent":        h.KeepRecent,
		"tool_result_tokens": h.ToolResultTokens,
		"max_result_bytes":   cfg.Tools.MaxResultBytes, // 0 ⇒ no ceiling
		// Reported so the form can say so rather than leaving the operator to
		// discover it from a provider error that names nothing about tools.
		"context_unguarded": s.engine().ContextUnguarded(),
		"defaults": map[string]int{
			"max_tokens":         history.DefaultMaxTokens,
			"keep_recent":        history.DefaultKeepRecent,
			"tool_result_tokens": history.DefaultToolResultTokens,
			"max_result_bytes":   0,
		},
	})
}

// handleSetHistory persists the history section and hot-reloads, so compaction
// retunes without a restart. Negative values are clamped rather than rejected.
func (s *Server) handleSetHistory(w http.ResponseWriter, r *http.Request) {
	var in historyInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	path, err := s.writeTargetPath()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := loadRawOrEmpty(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	if in.MaxTokens == nil {
		raw.History.MaxTokens = nil // fall back to the package default
	} else {
		n := max(0, *in.MaxTokens) // 0 = compaction off
		raw.History.MaxTokens = &n
	}
	raw.History.KeepRecent = max(0, in.KeepRecent)
	raw.History.ToolResultTokens = max(0, in.ToolResultTokens)
	raw.Tools.MaxResultBytes = max(0, in.MaxResultBytes)

	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_to": path})
}

// memorySearchInput toggles L2 session search (FTS5) from the web UI.
type memorySearchInput struct {
	Enabled bool `json:"enabled"`
	TopK    int  `json:"top_k"` // 0 = keep the existing value
}

// handleSetMemorySearch persists memory.search.enabled (and optional top_k) and
// hot-reloads, so L2 session search turns on/off from the dashboard without
// editing config.yaml or restarting. Enabling a never-configured search defaults
// the backend to "fts5" (the only one implemented; vector is L3).
func (s *Server) handleSetMemorySearch(w http.ResponseWriter, r *http.Request) {
	var in memorySearchInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	path, err := s.writeTargetPath()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := loadRawOrEmpty(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw.Memory.Search.Enabled = in.Enabled
	if in.Enabled && raw.Memory.Search.Backend == "" {
		raw.Memory.Search.Backend = "fts5"
	}
	if in.TopK > 0 {
		raw.Memory.Search.TopK = in.TopK
	}

	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"enabled":  s.engine().SearchEnabled(),
		"top_k":    s.engine().Config().Memory.Search.TopK,
		"saved_to": path,
	})
}

// persist saves the config and reloads the engine, writing an error response and
// returning a non-nil error if either step fails.
func (s *Server) persist(w http.ResponseWriter, c *config.Config, path string) error {
	if err := config.Save(c, path); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return err
	}
	if err := s.reload(); err != nil {
		writeErr(w, http.StatusInternalServerError, "saved but reload failed: "+err.Error())
		return err
	}
	return nil
}

// writeTargetPath returns the config file to edit: the file the running config
// came from, or the default user path when running from env/empty config.
func (s *Server) writeTargetPath() (string, error) {
	if p := s.engine().Config().SourcePath; p != "" && p != "(env)" {
		return p, nil
	}
	return config.DefaultUserConfigPath()
}

// loadRawOrEmpty loads a config without ${ENV} expansion, returning an empty
// config when the file does not exist yet.
func loadRawOrEmpty(path string) (*config.Config, error) {
	c, err := config.LoadRaw(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &config.Config{}, nil
		}
		return nil, err
	}
	return c, nil
}

func indexOfProvider(ps []config.Provider, name string) int {
	for i, p := range ps {
		if p.Name == name {
			return i
		}
	}
	return -1
}
