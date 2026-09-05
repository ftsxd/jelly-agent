#!/usr/bin/env python3
"""Measure what the read-back loop costs and accomplishes, on planted data.

Three groups, each run against the new path and against the baseline. The
baseline is not a different binary: it is the same one with no context_window
configured, which is exactly the behaviour before the dynamic budget existed —
no window means no budget to compute, so every payload goes into the prompt
whole. It is also what the deployed config looks like today.

Answers are planted in the synthetic log (see fake_logs_mcp.py) so "correct"
is checkable rather than eyeballed.
"""
import importlib.util, json, os, subprocess, sys, time, urllib.request
from datetime import datetime, timezone

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BENCH = os.path.join(ROOT, "bench")
OUT = os.environ.get("BENCH_OUT", os.path.join(BENCH, ".work"))
BIN = os.environ.get("BENCH_BIN", os.path.join(OUT, "jelly"))
FAKE = os.path.join(ROOT, "internal/engine/testdata/fake_logs_mcp.py")

# deepseek-v4-flash, USD per million tokens, from the vendor's pricing page
# (fetched 2026-09-04). Off-peak is half of peak; peak is 01:00-04:00 and
# 06:00-10:00 UTC, Monday to Friday.
#
# Written down rather than recalled: a price from memory is not something to
# put in front of someone as a bill.
PRICE_PEAK = {"cached": 0.014, "uncached": 0.44, "output": 1.32}

# Pre-seeded, not bootstrapped. BootstrapAdmin writes credentials back into
# the config file, the server watches that file, and the reload builds a fresh
# engine — which Closes the old one and cancels its MCP context, killing the
# log subprocess mid-run. Changing the password writes it a second time. Both
# showed up as the model diagnosing our own infrastructure instead of the logs.
ADMIN_PASSWORD = "bench-only-throwaway"
ADMIN_HASH = "$2a$10$ksgRefT7aWzLq3rQJDAu9.B7xPwuh.Pd7LnJiHyTl9P1885TpFwPG"


def price_now():
    t = datetime.now(timezone.utc)
    peak = t.weekday() < 5 and (1 <= t.hour < 4 or 6 <= t.hour < 10)
    f = 1.0 if peak else 0.5
    return {k: v * f for k, v in PRICE_PEAK.items()}, ("peak" if peak else "off-peak")


spec = importlib.util.spec_from_file_location("fake", FAKE)
fake = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fake)

SMALL, LARGE = 40, 60000
TIMEOUTS_SMALL = fake.timeout_count(SMALL)
TIMEOUTS_LARGE = fake.timeout_count(LARGE)

GROUPS = {
    "1-小结果": dict(
        lines=SMALL,
        ask="读取 payment-api 服务的日志，告诉我出现了什么错误、根本原因是什么。",
        want=["db-node-3"],
        note="全文轻松装得下：答案要对，且不应发生重读",
    ),
    "2-大结果找线索": dict(
        lines=LARGE,
        ask="读取 payment-api 服务的日志，找出故障的根本原因，以及最终是怎么恢复的。",
        # ROOT CAUSE at 50%, FINAL in the last three lines: reading only the
        # head misses the second while looking like it worked.
        want=["db-node-3", "db-node-5"],
        note="全文超预算，线索在中间和末尾：要靠搜索与局部读取找到",
    ),
    "3-大结果做统计": dict(
        lines=LARGE,
        ask="读取 payment-api 服务的日志，统计其中一共出现了多少次 TIMEOUT。只要准确的次数。",
        want=[str(TIMEOUTS_LARGE)],
        note="要求全量计数：工具全量数，模型拿到准确结果",
    ),
}

MODES = {
    "base": dict(context_window=0),          # 今天线上的样子：没有窗口，没有预算
    "new": dict(context_window=1_000_000),   # 声明窗口，动态预算生效
}


def write_config(path, lines, context_window):
    import yaml
    cfg = yaml.safe_load(open(os.path.expanduser("~/.jelly-agent/config.yaml"), encoding="utf-8"))
    cfg.pop("platforms", None)   # 会连真实钉钉
    cfg.pop("schedules", None)
    cfg["web"] = {"admin": {"username": "admin", "password_hash": ADMIN_HASH}}
    # Only the synthetic log server, so a real MCP backend cannot vary the run.
    cfg["mcp"] = [{
        "name": "fake-logs", "transport": "stdio",
        "command": sys.executable, "args": [FAKE],
        "env": {"FAKE_LOG_LINES": str(lines)},
        "enabled": True,
    }]
    if context_window:
        cfg["providers"][0]["context_window"] = context_window
    else:
        cfg["providers"][0].pop("context_window", None)
    yaml.safe_dump(cfg, open(path, "w", encoding="utf-8"), allow_unicode=True)


def post(url, body, cookie=None, timeout=900):
    req = urllib.request.Request(url, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    if cookie:
        req.add_header("Cookie", cookie)
    return urllib.request.urlopen(req, timeout=timeout)


def start(cfg_path, home, port):
    env = dict(os.environ, HOME=home, JELLY_CONFIG=cfg_path, JELLY_ADDR=f"127.0.0.1:{port}")
    log = open(os.path.join(home, "server.log"), "w")
    return subprocess.Popen([BIN], env=env, stdout=log, stderr=subprocess.STDOUT), log


def login(port):
    base = f"http://127.0.0.1:{port}"
    last = None
    for _ in range(80):
        time.sleep(0.5)
        try:
            r = post(base + "/api/auth/login",
                     {"username": "admin", "password": ADMIN_PASSWORD}, timeout=10)
            return r.headers.get("Set-Cookie", "").split(";")[0]
        except Exception as e:
            last = e
    raise RuntimeError(f"server never accepted a login: {last}")


def run_once(port, cookie, ask):
    """One turn in a fresh session."""
    t0 = time.time()
    frames = []
    try:
        r = post(f"http://127.0.0.1:{port}/api/chat/stream", {"message": ask}, cookie)
        for raw in r:
            line = raw.decode("utf-8", "replace").strip()
            if line.startswith("data:"):
                try:
                    frames.append(json.loads(line[5:].strip()))
                except Exception:
                    pass
    except Exception as e:
        return dict(ok=False, fail=f"{type(e).__name__}: {e}", secs=round(time.time() - t0, 1),
                    answer="", tools=[], readers=[], rounds=0, prompt=0,
                    cached=0, uncached=0, output=0, withheld=0)

    tools, readers, turns, text, err = [], [], [], [], []
    withheld = 0
    for f in frames:
        t = f.get("type")
        if t == "tool_call":
            n = f.get("name")
            tools.append(n)
            if n in ("read_result", "search_result"):
                readers.append(n)
        elif t == "tool_result":
            ov = (f.get("response") or {}).get("overview") or {}
            if "本轮剩余预算不足" in str(ov.get("note", "")):
                withheld += 1
        elif t == "llm_turn":
            turns.append(f)
        elif t == "text":
            text.append(f.get("text", ""))
        elif t == "error":
            err.append(json.dumps(f, ensure_ascii=False))

    done = next((f for f in reversed(frames) if f.get("type") == "done"), None)
    u = (done or {}).get("usage") or {}
    prompt, cached = u.get("prompt", 0) or 0, u.get("cached", 0) or 0
    return dict(ok=not err and bool(text), fail="; ".join(err)[:400],
                secs=round(time.time() - t0, 1), answer="".join(text).strip(),
                tools=tools, readers=readers, rounds=len(turns),
                prompt=prompt, cached=cached, uncached=max(prompt - cached, 0),
                output=u.get("completion", 0) or 0, withheld=withheld)


def cost_of(m, price):
    return (m["cached"] * price["cached"] + m["uncached"] * price["uncached"]
            + m["output"] * price["output"]) / 1_000_000


def main():
    runs = int(os.environ.get("BENCH_RUNS", "3"))
    only = os.environ.get("BENCH_ONLY", "")
    price, window = price_now()
    os.makedirs(OUT, exist_ok=True)
    print(f"# 计价 {window}: cached=${price['cached']}/M uncached=${price['uncached']}/M output=${price['output']}/M")
    print(f"# 植入答案: 小 TIMEOUT={TIMEOUTS_SMALL}  大 TIMEOUT={TIMEOUTS_LARGE}", flush=True)

    results, port = [], 19100
    for gname, g in GROUPS.items():
        if only and only not in gname:
            continue
        for mode, mcfg in MODES.items():
            port += 1
            home = os.path.join(OUT, f"home_{gname}_{mode}")
            subprocess.run(["rm", "-rf", home])
            os.makedirs(home)
            cfg = os.path.join(home, "config.yaml")
            write_config(cfg, g["lines"], mcfg["context_window"])
            proc, log = start(cfg, home, port)
            try:
                cookie = login(port)
                for i in range(runs):
                    m = run_once(port, cookie, g["ask"])
                    m["correct"] = m["ok"] and all(w in m["answer"] for w in g["want"])
                    m["cost"] = cost_of(m, price)
                    m.update(group=gname, mode=mode, run=i + 1, lines=g["lines"])
                    results.append(m)
                    print(f"  {gname:16s} {mode:4s} #{i+1}  正确={'✓' if m['correct'] else '✗'} "
                          f"轮={m['rounds']} 工具={len(m['tools'])} 读回={len(m['readers'])} "
                          f"扣留={m['withheld']} cached={m['cached']} uncached={m['uncached']} "
                          f"out={m['output']} ${m['cost']:.5f} {m['secs']}s"
                          + (f"  失败: {m['fail'][:140]}" if not m["ok"] else ""), flush=True)
            finally:
                proc.terminate()
                try:
                    proc.wait(timeout=10)
                except Exception:
                    proc.kill()
                log.close()

    path = os.path.join(BENCH, "results.json")
    json.dump(dict(price=price, window=window, runs=runs, at=datetime.now().isoformat(timespec="seconds"),
                   planted=dict(small_timeouts=TIMEOUTS_SMALL, large_timeouts=TIMEOUTS_LARGE),
                   groups={k: v["note"] for k, v in GROUPS.items()},
                   results=results), open(path, "w", encoding="utf-8"), ensure_ascii=False, indent=1)
    print("\n写入", path)


if __name__ == "__main__":
    main()
