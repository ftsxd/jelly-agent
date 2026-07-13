package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"golang.org/x/crypto/bcrypt"
)

const (
	authCookieName = "jelly_admin_session"
	authSessionTTL = 12 * time.Hour
)

type authSession struct {
	username     string
	passwordHash string
	expiresAt    time.Time
}

type authManager struct {
	mu       sync.Mutex
	sessions map[string]authSession
}

func newAuthManager() *authManager { return &authManager{sessions: make(map[string]authSession)} }

// ValidateAdmin rejects configurations that would leave the dashboard
// inaccessible. The executable entry points call it before binding a socket.
func ValidateAdmin(admin config.Admin) error {
	if !admin.Configured() {
		return fmt.Errorf("Web 控制台管理员尚未初始化")
	}
	if _, err := bcrypt.Cost([]byte(admin.PasswordHash)); err != nil {
		return fmt.Errorf("web.admin.password_hash 必须是 bcrypt 哈希；请重新运行 jelly admin set-password: %w", err)
	}
	return nil
}

// BootstrapAdmin creates the initial local administrator when none exists.
// Its one-time password is returned to the caller so it can be displayed only
// on the server's terminal; only a bcrypt hash is persisted.
func BootstrapAdmin(cfg *config.Config) (string, error) {
	if cfg.Web.Admin.Configured() {
		return "", ValidateAdmin(cfg.Web.Admin)
	}
	path := cfg.SourcePath
	if path == "" || path == "(env)" {
		var err error
		path, err = config.DefaultUserConfigPath()
		if err != nil {
			return "", err
		}
	}
	raw, err := config.LoadRaw(path)
	if os.IsNotExist(err) {
		raw = &config.Config{}
	} else if err != nil {
		return "", err
	}
	// Another invocation may have written credentials after the caller loaded
	// cfg; never silently replace an existing administrator.
	if raw.Web.Admin.Configured() {
		return "", ValidateAdmin(raw.Web.Admin)
	}
	password, err := newInitialPassword()
	if err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("生成初始管理员密码哈希: %w", err)
	}
	raw.Web.Admin = config.Admin{Username: "admin", PasswordHash: string(hash), MustChange: true}
	if err := config.Save(raw, path); err != nil {
		return "", err
	}
	return password, nil
}

type loginInput struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Static assets must remain reachable so the login page can load. Health
		// is deliberately public for local process checks; all other API routes
		// require the configured administrator session.
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/health" ||
			r.URL.Path == "/api/auth/status" || r.URL.Path == "/api/auth/login" || r.URL.Path == "/api/auth/logout" || r.URL.Path == "/api/auth/password" {
			next.ServeHTTP(w, r)
			return
		}
		if !s.engine().Config().Web.Admin.Configured() {
			// New is also used by API-only unit tests and local embedding. Both
			// executable server entry points reject this configuration before they
			// bind a socket; retaining this path keeps Server usable as a library.
			next.ServeHTTP(w, r)
			return
		}
		if !s.auth.authenticated(r, s.engine().Config().Web.Admin.Username, s.engine().Config().Web.Admin.PasswordHash) {
			writeErr(w, http.StatusUnauthorized, "请先以管理员身份登录")
			return
		}
		if s.engine().Config().Web.Admin.MustChange {
			writeErr(w, http.StatusForbidden, "首次登录后请先修改管理员密码")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	admin := s.engine().Config().Web.Admin
	if !admin.Configured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"configured": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":    true,
		"authenticated": s.auth.authenticated(r, admin.Username, admin.PasswordHash),
		"username":      admin.Username,
		"must_change":   admin.MustChange,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	admin := s.engine().Config().Web.Admin
	if !admin.Configured() {
		writeErr(w, http.StatusServiceUnavailable, "控制台管理员尚未配置；请运行 jelly admin set-password")
		return
	}
	var in loginInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "无效的登录请求")
		return
	}
	if strings.TrimSpace(in.Username) != admin.Username || bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(in.Password)) != nil {
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	token, err := newSessionToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "无法创建登录会话")
		return
	}
	s.auth.mu.Lock()
	s.auth.sessions[token] = authSession{username: admin.Username, passwordHash: admin.PasswordHash, expiresAt: time.Now().Add(authSessionTTL)}
	s.auth.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: authCookieName, Value: token, Path: "/", MaxAge: int(authSessionTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": admin.Username, "must_change": admin.MustChange})
}

type changePasswordInput struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	admin := s.engine().Config().Web.Admin
	if !s.auth.authenticated(r, admin.Username, admin.PasswordHash) {
		writeErr(w, http.StatusUnauthorized, "请先以管理员身份登录")
		return
	}
	var in changePasswordInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "无效的改密请求")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(in.CurrentPassword)) != nil {
		writeErr(w, http.StatusUnauthorized, "当前密码错误")
		return
	}
	if len([]rune(strings.TrimSpace(in.NewPassword))) < 12 {
		writeErr(w, http.StatusBadRequest, "新密码至少需要 12 个字符")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "无法保存新密码")
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
	raw.Web.Admin = config.Admin{Username: admin.Username, PasswordHash: string(hash)}
	if err := s.persist(w, raw, path); err != nil {
		return
	}
	// Rotate the session as well as the password so any captured old cookie is
	// invalid immediately. Browsers apply the replacement Set-Cookie response.
	if token, err := newSessionToken(); err == nil {
		if old, err := r.Cookie(authCookieName); err == nil {
			s.auth.mu.Lock()
			delete(s.auth.sessions, old.Value)
			s.auth.sessions[token] = authSession{username: admin.Username, passwordHash: string(hash), expiresAt: time.Now().Add(authSessionTTL)}
			s.auth.mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: authCookieName, Value: token, Path: "/", MaxAge: int(authSessionTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(authCookieName); err == nil {
		s.auth.mu.Lock()
		delete(s.auth.sessions, c.Value)
		s.auth.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: authCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *authManager) authenticated(r *http.Request, username, passwordHash string) bool {
	c, err := r.Cookie(authCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	session, ok := a.sessions[c.Value]
	if !ok || time.Now().After(session.expiresAt) || session.username != username || session.passwordHash != passwordHash {
		delete(a.sessions, c.Value)
		return false
	}
	return true
}

func newSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func newInitialPassword() (string, error) {
	// Base64 URL encoding gives an easily copyable, 22-character secret without
	// shell-sensitive punctuation. It is generated from 128 bits of randomness.
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
