package tool

import (
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/loadmemorytool"

	"github.com/jelly-agent/jelly-agent/internal/memory"
)

// Builtins returns the built-in tool instances registered for agents. When
// core is non-nil the L1 core-memory tools (remember/forget) are included; when
// withSearch is true the ADK load_memory tool is added for L2 session search
// (it requires the runner's MemoryService to be set — see PLAN §10.1).
//
// web_search and fetch_url are always present (both read-only network reads;
// fetch_url refuses non-public addresses). run_code / read_file / write_file /
// query_db land later, behind the permission model (see PLAN §8 risk 6).
func Builtins(core *memory.Core, withSearch bool) ([]adktool.Tool, error) {
	ws, err := NewWebSearchTool()
	if err != nil {
		return nil, err
	}
	fetch, err := NewFetchURLTool()
	if err != nil {
		return nil, err
	}
	tools := []adktool.Tool{ws, fetch}
	if core != nil {
		mem, err := MemoryTools(core)
		if err != nil {
			return nil, err
		}
		tools = append(tools, mem...)
	}
	if withSearch {
		tools = append(tools, loadmemorytool.New())
	}
	return tools, nil
}
