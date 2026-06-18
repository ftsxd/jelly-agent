package server

import (
	"net/http"
	"os"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/config"
)

// providerInput is the body for POST /api/providers (create or update).
type providerInput struct {
	Name        string `json:"name"`
	BaseURL     string `json:"base_url"`
	APIKey      string `json:"api_key"` // empty on update = keep existing key
	Model       string `json:"model"`
	MakeDefault bool   `json:"make_default"`
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
		raw.Providers = append(raw.Providers, config.Provider{
			Name: in.Name, BaseURL: in.BaseURL, APIKey: in.APIKey, Model: in.Model,
		})
	} else {
		p := &raw.Providers[idx]
		p.BaseURL = in.BaseURL
		p.Model = in.Model
		if strings.TrimSpace(in.APIKey) != "" {
			p.APIKey = in.APIKey // only overwrite when a new key is supplied
		}
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
