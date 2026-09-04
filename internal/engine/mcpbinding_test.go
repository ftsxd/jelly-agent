package engine

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/agent"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/toolconfirmation"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/gateway"
	jellymcp "github.com/jelly-agent/jelly-agent/internal/mcp"
	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/toolreg"
)

// Two real MCP servers, spoken to over real stdio through ADK's own
// mcptoolset.
//
// This exists because the unit fakes could not have caught what it caught.
// ADK's mcptoolset reports the constant "mcp_tool_set" as its name for every
// instance, while a hand-written fake naturally reports its own — so binding
// by set.Name() looked correct in unit tests and, against two real servers,
// put both in one executor slot: the second registration took over the first
// server's traffic and their duplicate tool names read as one tool being
// re-bound. Nothing errored; the calls went to the wrong cluster.
//
// The fixture server echoes which instance answered, so the decisive
// assertion is about where a call landed rather than about how it was named.
func TestMCPToolsAreBoundPerServer(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available for the MCP fixture server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reg, _ := toolreg.Build(nil) // nothing declared: the MCP case in practice
	gw := gateway.New(gateway.Config{
		Registry: gateway.Snapshot(reg),
		Policy:   gateway.Policy{MaxSideEffect: ops.SideEffectMutating},
	})
	undeclared := map[string][]string{}
	binder := &gateway.Binder{
		GW: gw, Registry: gateway.Snapshot(reg), Fallback: New(&config.Config{}).undeclaredFallback(),
		Report: func(server string, names []string) { undeclared[server] = names },
	}

	bind := func(server string) []adktool.Tool {
		t.Helper()
		set, err := jellymcp.Toolset(ctx, config.MCPServer{
			Name: server, Transport: "stdio", Command: "python3",
			Args:    []string{"testdata/fake_mcp.py"},
			Env:     map[string]string{"FAKE_MCP_LABEL": server},
			Enabled: true,
		})
		if err != nil {
			t.Fatalf("%s: %v", server, err)
		}
		tools, err := binder.Toolset(server, set).Tools(&agent.StrictContextMock{Ctx: ctx})
		if err != nil {
			t.Fatalf("%s tools: %v", server, err)
		}
		return tools
	}

	a, b := bind("k8s-a"), bind("k8s-b")

	// Every MCP tool must be on the gateway's path — that is the claim the
	// whole design rests on, and it was false for MCP until now.
	byName := map[string]*gateway.Wrapped{}
	for server, tools := range map[string][]adktool.Tool{"k8s-a": a, "k8s-b": b} {
		for _, tl := range tools {
			w, ok := tl.(*gateway.Wrapped)
			if !ok {
				t.Fatalf("%s/%s bypassed the gateway (%T)", server, tl.Name(), tl)
			}
			byName[w.Name()] = w
		}
	}

	// The first server keeps the names its tools were given; only the clashing
	// second one is prefixed, because a mangled name costs selection accuracy.
	for _, want := range []string{"get_pods", "delete_pod", "k8s-b__get_pods", "k8s-b__delete_pod"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("no tool named %q; got %v", want, keysOf(byName))
		}
	}

	// The decisive check: each name reaches its own server.
	for name, wantLabel := range map[string]string{"get_pods": "k8s-a", "k8s-b__get_pods": "k8s-b"} {
		w, ok := byName[name]
		if !ok {
			continue
		}
		res, err := w.Run(&mcpToolCtx{agent.StrictContextMock{Ctx: ctx}}, map[string]any{"ns": "default"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := flatten(res); !strings.Contains(got, wantLabel+":") {
			t.Errorf("%s landed on the wrong server: %s", name, got)
		}
	}

	// And the gap is reported per server, not once for whichever bound first.
	if len(undeclared) != 2 {
		t.Errorf("undeclared reported for %v, want both servers named", keysOfStrings(undeclared))
	}
}

type mcpToolCtx struct{ agent.StrictContextMock }

func (c *mcpToolCtx) SessionID() string      { return "s" }
func (c *mcpToolCtx) InvocationID() string   { return "i" }
func (c *mcpToolCtx) AgentName() string      { return "jelly" }
func (c *mcpToolCtx) FunctionCallID() string { return "f" }

// nil means "nothing pending", which is what a real invocation carries for a
// tool that does not require confirmation. mcpTool asks for this on every run.
func (c *mcpToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

func flatten(m map[string]any) string {
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for _, iv := range t {
				walk(iv)
			}
		case []any:
			for _, iv := range t {
				walk(iv)
			}
		default:
			b.WriteString(strings.TrimSpace(strings.Join(strings.Fields(strings.TrimSpace(toString(t))), " ")))
			b.WriteString(" ")
		}
	}
	walk(m)
	return b.String()
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

func keysOf(m map[string]*gateway.Wrapped) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOfStrings(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
