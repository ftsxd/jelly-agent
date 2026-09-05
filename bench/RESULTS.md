# 对照表（修完复核提出的四条之后）

```
deepseek-v4-flash（off-peak）  缓存输入 $0.007/M ｜ 未缓存 $0.22/M ｜ 输出 $0.66/M
每格 3 次取中位数 ｜ 植入答案：大日志 TIMEOUT=1622 次 ｜ 2026-09-05T22:45:49

测试               模式        正确   轮   工具   读回   扣留     缓存输入     未缓存     输出      费用/次     耗时
────────────────────────────────────────────────────────────────────────────────────────────────
  全文轻松装得下：答案要对，且不应发生重读
1-小结果            基线       3/3   2    1    0    0     2816    1593    786  $0.00075   5.9s
1-小结果            新        3/3   2    1    0    0     2816    1575    753  $0.00086   5.8s

  全文超预算，线索在中间和末尾：要靠搜索与局部读取找到
2-大结果找线索         基线       0/3   —    —    —    —        —       —      —  $0.00000   5.6s  ← 无法完成
2-大结果找线索         新        3/3  12   19   18    1   195712   25150   8466  $0.01249  74.0s

  要求全量计数：工具全量数，模型拿到准确结果
3-大结果做统计         基线       0/3   —    —    —    —        —       —      —  $0.00000   6.0s  ← 无法完成
3-大结果做统计         新        3/3   3    2    1    1     4736    1546    222  $0.00052   3.1s

失败原因：
  2-大结果找线索 / base: {"message": "openai chat stream: error, status code: 400, status: 400 Bad Request, message: This model's maximum context length is 1048576 tokens. However, you requested 1721962 tokens (1721962 in the messages, 0 in the completion). Please 
  3-大结果做统计 / base: {"message": "openai chat stream: error, status code: 400, status: 400 Bad Request, message: This model's maximum context length is 1048576 tokens. However, you requested 1721957 tokens (1721957 in the messages, 0 in the completion). Please 
```

## 与修复前的对照

| 测试 | 修复前 费用/次 | 修复后 费用/次 | 修复前 工具 | 修复后 工具 | 正确 |
|---|---|---|---|---|---|
| 1-小结果 新 | $0.00086 | $0.00086 | 1 | 1 | 3/3 → 3/3 |
| 2-大结果找线索 新 | $0.00732 | $0.01249 | 12 | 19 | 3/3 → 3/3 |
| 3-大结果做统计 新 | $0.00059 | $0.00052 | 2 | 2 | 3/3 → 3/3 |

第 2 组两次采样分别是 11/12/19 与 17/19/19 次工具调用，跨度都很大，
n=3 不足以把成本上升归因到这次改动；它本来就是我先前标出的"搜索策略有波动"那一格。
可以排除的是扣留路径 失灵：预览只在放不下时才丢，而这一组首轮预算是窗口的 25%（25 万 token），
扣留响应约 333 token，远未触及（见 TestAVeryTightRoundDropsThePreviewButKeepsTheHandle 与
TestAnOversizedResultArrivesAsAnOverviewNotACutObject）。
