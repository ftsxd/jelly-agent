#!/usr/bin/env python3
"""MCP stdio server serving synthetic service logs with planted answers.

For measuring what the delivery store, the dynamic budget and the read-back
tools actually cost and accomplish, without waiting on a real MCP backend and
without a real backend's shifting data — the answers have to be known in
advance or "correct" is not checkable.

Three facts are planted, each aimed at a different failure mode:

  ROOT CAUSE  at ~50% of the file, so finding it requires reaching the middle
              rather than reading the head.
  FINAL       in the last few lines, so a strategy that reads only the head
              misses it while looking like it worked.
  TIMEOUT     an exact number of times, so a count can be checked rather
              than eyeballed. Counting it by reading is expensive on purpose:
              the occurrences are spread across the whole file.

FAKE_LOG_LINES sets the size, so the same server serves the small and large
cases and the only difference between runs is the one under test.
"""
import json, os, sys

LINES = int(os.environ.get("FAKE_LOG_LINES", "40"))
SEED = int(os.environ.get("FAKE_LOG_SEED", "7"))

ROOT_CAUSE = "ROOT CAUSE: connection pool exhausted on db-node-3 (max=64, waiting=311)"
FINAL = "FINAL: failover completed to db-node-5, error rate back to 0.02%"

# TIMEOUT lines are placed on a fixed stride so the count is exact and
# independent of LINES arithmetic drift.
TIMEOUT_STRIDE = 37


def timeout_count(n):
    """How many TIMEOUT lines a log of n lines contains."""
    return len([i for i in range(n) if i % TIMEOUT_STRIDE == 0 and i not in _planted(n)])


def _planted(n):
    return {n // 2, n - 3}


def build(n):
    planted = _planted(n)
    out = []
    for i in range(n):
        ts = f"2026-09-04T{(i // 3600) % 24:02d}:{(i // 60) % 60:02d}:{i % 60:02d}Z"
        if i == n // 2:
            out.append(f"{ts} ERROR payment-api {ROOT_CAUSE}")
        elif i == n - 3:
            out.append(f"{ts} INFO  payment-api {FINAL}")
        elif i % TIMEOUT_STRIDE == 0:
            out.append(f"{ts} WARN  payment-api TIMEOUT upstream=db-node-3 waited=3000ms attempt={i // TIMEOUT_STRIDE + 1}")
        elif i % 11 == 0:
            out.append(f"{ts} INFO  payment-api request id=req-{SEED}{i:06d} status=200 latency={12 + i % 40}ms")
        else:
            out.append(f"{ts} DEBUG payment-api cache lookup key=order:{SEED}{i:06d} hit={'true' if i % 3 else 'false'}")
    return "\n".join(out) + "\n"


TOOLS = [{
    "name": "get_service_logs",
    "description": "读取某个服务的应用日志全文。",
    "inputSchema": {
        "type": "object",
        "properties": {"service": {"type": "string", "description": "服务名，例如 payment-api"}},
        "required": ["service"],
    },
}]


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
                "protocolVersion": "2024-11-05",
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "fake-logs", "version": "0.0.1"}}})
        elif method == "tools/list":
            send({"jsonrpc": "2.0", "id": rid, "result": {"tools": TOOLS}})
        elif method == "tools/call":
            name = (req.get("params") or {}).get("name")
            if name != "get_service_logs":
                send({"jsonrpc": "2.0", "id": rid,
                      "error": {"code": -32601, "message": f"no such tool: {name}"}})
                continue
            body = build(LINES)
            send({"jsonrpc": "2.0", "id": rid, "result": {
                "content": [{"type": "text", "text": body}], "isError": False}})
        elif rid is not None:
            send({"jsonrpc": "2.0", "id": rid, "error": {"code": -32601, "message": "unsupported"}})


if __name__ == "__main__":
    main()
