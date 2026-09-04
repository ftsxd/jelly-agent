#!/usr/bin/env python3
"""Minimal MCP stdio server, for verifying the gateway's MCP path.

Exposes get_pods (harmless, read-only in spirit) and delete_pod. Two instances
run under different server names reproduce the duplicate-name case.
"""
import json, os, sys

LABEL = os.environ.get("FAKE_MCP_LABEL", "srv")

def _tool(name, description, prop="ns"):
    return {"name": name, "description": description,
            "inputSchema": {"type": "object", "properties": {prop: {"type": "string"}}}}


# Enough tools, with distinct subjects, that a budget can meaningfully cut.
TOOLS = [
    _tool("get_pods", f"list pods on {LABEL}"),
    _tool("delete_pod", f"delete a pod on {LABEL}", "name"),
    _tool("get_logs", f"read container logs on {LABEL}"),
    _tool("describe_node", f"describe a node on {LABEL}", "node"),
    _tool("query_mysql", f"run a read-only MySQL query on {LABEL}", "sql"),
    _tool("list_deployments", f"list deployments on {LABEL}"),
]

def send(msg):
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()

def main():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except Exception:
            continue
        method, rid = req.get("method"), req.get("id")
        if method == "initialize":
            send({"jsonrpc": "2.0", "id": rid, "result": {
                "protocolVersion": req.get("params", {}).get("protocolVersion", "2024-11-05"),
                "capabilities": {"tools": {}},
                "serverInfo": {"name": f"fake-{LABEL}", "version": "0.0.1"}}})
        elif method == "tools/list":
            send({"jsonrpc": "2.0", "id": rid, "result": {"tools": TOOLS}})
        elif method == "tools/call":
            p = req.get("params", {})
            send({"jsonrpc": "2.0", "id": rid, "result": {
                "content": [{"type": "text",
                             "text": f"{LABEL}:{p.get('name')} args={json.dumps(p.get('arguments', {}), sort_keys=True)}"}],
                "isError": False}})
        elif rid is not None:
            send({"jsonrpc": "2.0", "id": rid, "error": {"code": -32601, "message": method}})
        # notifications (no id) need no reply

main()
