#!/usr/bin/env python3
"""Turn results.json into the comparison table."""
import json, os, statistics as st

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
d = json.load(open(os.path.join(ROOT, "bench", "results.json"), encoding="utf-8"))
rows = d["results"]
for r in rows:
    r["tools_n"], r["readers_n"] = len(r["tools"]), len(r["readers"])


def agg(group, mode):
    rs = [r for r in rows if r["group"] == group and r["mode"] == mode]
    if not rs:
        return None
    ok = [r for r in rs if r["ok"]] or rs

    def med(k):
        return int(st.median([r[k] for r in ok]))

    return dict(n=len(rs), correct=sum(1 for r in rs if r["correct"]),
                failed=sum(1 for r in rs if not r["ok"]),
                rounds=med("rounds"), tools=med("tools_n"), readers=med("readers_n"),
                withheld=med("withheld"), cached=med("cached"), uncached=med("uncached"),
                output=med("output"),
                cost=st.median([r["cost"] for r in ok]),
                secs=st.median([r["secs"] for r in rs]))


price, win = d["price"], d["window"]
print(f"deepseek-v4-flash（{win}）  缓存输入 ${price['cached']}/M ｜ 未缓存 ${price['uncached']}/M ｜ 输出 ${price['output']}/M")
print(f"每格 {d['runs']} 次取中位数 ｜ 植入答案：大日志 TIMEOUT={d['planted']['large_timeouts']} 次 ｜ {d.get('at','')}")
print()
hdr = (f"{'测试':17s}{'模式':6s}{'正确':>6s}{'轮':>4s}{'工具':>5s}{'读回':>5s}{'扣留':>5s}"
       f"{'缓存输入':>9s}{'未缓存':>8s}{'输出':>7s}{'费用/次':>10s}{'耗时':>7s}")
print(hdr)
print("─" * 96)
for g in sorted({r["group"] for r in rows}):
    print(f"  {d['groups'].get(g, '')}")
    for mode, label in (("base", "基线"), ("new", "新")):
        a = agg(g, mode)
        if not a:
            continue
        if a["failed"] == a["n"]:
            print(f"{g:17s}{label:6s}{'0/%d' % a['n']:>6s}{'—':>4s}{'—':>5s}{'—':>5s}{'—':>5s}"
                  f"{'—':>9s}{'—':>8s}{'—':>7s}{'$0.00000':>10s}{str(a['secs'])+'s':>7s}  ← 无法完成")
        else:
            print(f"{g:17s}{label:6s}{'%d/%d' % (a['correct'], a['n']):>6s}{a['rounds']:>4d}"
                  f"{a['tools']:>5d}{a['readers']:>5d}{a['withheld']:>5d}{a['cached']:>9d}"
                  f"{a['uncached']:>8d}{a['output']:>7d}{'$%.5f' % a['cost']:>10s}{str(a['secs'])+'s':>7s}")
    print()

fails = {}
for r in rows:
    if not r["ok"] and r["fail"]:
        fails.setdefault((r["group"], r["mode"]), r["fail"].replace("\n", " "))
if fails:
    print("失败原因：")
    for (g, m), msg in fails.items():
        print(f"  {g} / {m}: {msg[:240]}")

wrong = [r for r in rows if r["ok"] and not r["correct"]]
if wrong:
    print("\n答完但不正确的：")
    for r in wrong:
        print(f"  {r['group']}/{r['mode']} #{r['run']}: …{r['answer'][-160:]}")
