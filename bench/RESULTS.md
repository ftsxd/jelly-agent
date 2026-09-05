# 对照表

```
deepseek-v4-flash（off-peak）  缓存输入 $0.007/M ｜ 未缓存 $0.22/M ｜ 输出 $0.66/M
每格 3 次取中位数 ｜ 植入答案：大日志 TIMEOUT=1622 次 ｜ 2026-09-05T22:19:27

测试               模式        正确   轮   工具   读回   扣留     缓存输入     未缓存     输出      费用/次     耗时
────────────────────────────────────────────────────────────────────────────────────────────────
  全文轻松装得下：答案要对，且不应发生重读
1-小结果            基线       3/3   2    1    0    0     2816    1587    667  $0.00081   5.3s
1-小结果            新        3/3   2    1    0    0     2816    1587    749  $0.00086   5.5s

  全文超预算，线索在中间和末尾：要靠搜索与局部读取找到
2-大结果找线索         基线       0/3   —    —    —    —        —       —      —  $0.00000   6.3s  ← 无法完成
2-大结果找线索         新        3/3   8   12   11    1    61696   14514   5600  $0.00732  45.4s

  要求全量计数：工具全量数，模型拿到准确结果
3-大结果做统计         基线       0/3   —    —    —    —        —       —      —  $0.00000   6.3s  ← 无法完成
3-大结果做统计         新        3/3   3    2    1    1     4864    1875    230  $0.00059   2.5s

失败原因：
  2-大结果找线索 / base: {"message": "openai chat stream: error, status code: 400, status: 400 Bad Request, message: This model's maximum context length is 1048576 tokens. However, you requested 1721956 tokens (1721956 in the messages, 0 in the completion). Please 
  3-大结果做统计 / base: {"message": "openai chat stream: error, status code: 400, status: 400 Bad Request, message: This model's maximum context length is 1048576 tokens. However, you requested 1721949 tokens (1721949 in the messages, 0 in the completion). Please 
```
