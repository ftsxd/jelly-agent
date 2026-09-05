# 上线配置：两步，缺一步等于没做

动态结果预算靠 `context_window` 才能算出"本轮还剩多少"。但只加它不够：
显式的 `history.max_tokens` 优先于按窗口推导的值，所以一个大于窗口的
历史预算会让压缩永不触发，会话照样涨到超窗被 provider 拒绝。

线上现状（`~/.jelly-agent/config.yaml`）：

    history:
      max_tokens: 10000000     # ← 一千万，而窗口是一百万
    providers:
      - name: deepseek
        model: deepseek-v4-flash
        # 没有 context_window   # ← 动态预算完全不生效

## 改成

    history:
      # 删掉 max_tokens，按 context_window 的 60% 推导（= 60 万）
      # 或显式给一个不超过窗口的值，例如 max_tokens: 600000

    providers:
      - name: deepseek
        model: deepseek-v4-flash
        context_window: 1000000

两处都改完，才是对照表里"新"那一列的行为。只改一处：

| 只加 context_window | 只删 history.max_tokens | 都不改 |
|---|---|---|
| 单次大结果被扣留，但历史无上限，长会话仍会超窗 | 历史会压缩，但大结果整份进 prompt，一次就超窗 | 对照表里"基线"那一列——大结果 0/3 |

## 一道保险

代码现在会拦下"历史预算大于窗口"这种配置：它不是"晚点再压缩"，
而是"永不压缩"，而"永不压缩"已经有写法了（显式 0）。所以大于窗口的值
一定是笔误，日志会警告并改用推导值。这只是保险，不是替代——
`context_window` 仍然必须自己填。

## 验证

    BENCH_RUNS=3 python3 bench/harness.py && python3 bench/report.py

小结果三次全对且零重读、大结果三次全对，就是配置生效了。
