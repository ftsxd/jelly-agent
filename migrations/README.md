# 存储迁移：SQLite → MySQL + Elasticsearch

```
migrations/
├── mysql/0001_init.sql               手写的五张业务表 + 两张新表
└── elastic/
    ├── jelly-memory.json             替代 memory_fts（需 IK 插件）
    ├── jelly-memory.no-ik.json       同上，改用内置 cjk 二元分词
    ├── jelly-artifacts.json          大产物全文检索（需 IK 插件）
    └── jelly-artifacts.no-ik.json    同上，无插件版
```

## ADK 的四张表不在这里

`sessions` / `events` / `app_states` / `user_states` 由 ADK 自己的 GORM 模型
建（`internal/session/sqlite.go` → `database.AutoMigrate`）。**不要手写一份
DDL**：ADK 升级时改了模型，AutoMigrate 会跟上，手写的那份只会静默漂移。

换 MySQL 就是换一个 dialector：

```go
database.NewSessionService(mysql.Open(dsn), &gorm.Config{...})
```

ADK 明确支持（`session/database/gorm_datatypes.go` 里有 postgres / mysql /
spanner 三个分支）。但有一件事要盯：

> `storageEvent` 的主键是 4 个无 size 的 `string`。GORM 的 MySQL 驱动把
> 主键上的无 size string 映射成 `varchar(191)`
> （`gorm.io/driver/mysql@v1.6.0/mysql.go:392`），于是
> **4 × 191 × 4 = 3056 字节**，而 InnoDB 索引键上限是 3072。
>
> **能建，只剩 16 字节余量。** ADK 哪天给主键加第五列，AutoMigrate 当场失败。
> 而且这是 events 表的聚簇主键，量最大的那张，每条二级索引都背着它。
>
> 这个数是从驱动源码算出来的，**上线前用真库跑一次 AutoMigrate 确认**。

## MySQL 参数

```ini
[mysqld]
max_allowed_packet = 64M      # 产物实测已有 82KB，LONGBLOB 单行要能过网
innodb_file_per_table = ON
character_set_server = utf8mb4
collation_server = utf8mb4_0900_ai_ci
```

连接串至少要带：

```
?charset=utf8mb4&parseTime=true&loc=UTC&interpolateParams=false
```

`parseTime=true` 是必须的——DATETIME(6) 要能直接扫进 `time.Time`。
`loc=UTC` 也是：现在所有时间都按 UTC 存，换机器时区不能让语义漂移。

## 和 SQLite 版的结构差异，以及为什么

| 差异 | 原因 |
|---|---|
| `tool_results` 换代理主键 | 原来是 5 个 TEXT 列做主键，MySQL 里 TEXT 不加前缀长度不能进索引。换 VARCHAR 后能建，但 InnoDB 聚簇，二级索引都背这个宽键。 |
| `payload` → `LONGBLOB` | MySQL 的 `BLOB` 只有 64KB，现网实测单条产物已 82,540 字节。 |
| `at` → `DATETIME(6)` | 原来存 RFC3339Nano 文本，而这个格式砍掉末尾的零：整秒的 `…T10:00:00Z` 在字符串比较里排在同秒的 `…T10:00:00.5Z` **之后**（`'.' < 'Z'`）。`ORDER BY at` 在秒内是乱的。换真时间类型顺带修掉。 |
| `expired_at` → 可空 | 原来用空字符串表示未过期，因为 SQLite 那列是 NOT NULL。 |
| 新增 `tool_result_seq` | 原来的 seq 分配写在 INSERT 的子查询里，引用了插入目标表 —— MySQL error 1093。改用 `LAST_INSERT_ID` 序列惯用法，仍是一条语句、仍原子。 |
| 新增 `tool_calls(session_id, invocation_id)` 索引 | 任务中心按一次运行取全部调用。本地文件上过滤一下无所谓，走网络值得让索引一次带出。 |
| 标识列 `ascii_bin` | 见下。 |

## 两条容易踩死的规定

**① 标识列必须显式 `_bin` 排序规则。**

SQLite 的 TEXT 默认按 BINARY 比较，区分大小写。MySQL 默认的
`utf8mb4_0900_ai_ci` **不区分大小写、也不区分重音**。照搬默认值会让两个本来
不同的 `call_id` 撞成一个——`'A1b2'` 和 `'a1B2'` 在默认排序规则下相等，而
它们是唯一键的一部分。

**② 标识列用 `ascii` 而不是 `utf8mb4`。**

session_id / invocation_id / call_id / sha256 全是 UUID、十六进制或生成的短
id，永不含非 ASCII。ascii 每字符 1 字节，utf8mb4 是 4 字节。这是 5 列复合
唯一键能舒服地待在 InnoDB 3072 字节上限之内的原因（320 字节 vs 1280 字节）。

现网实测的最大长度，VARCHAR 宽度是照它给的余量：

| 列 | 实测最长 | 给到 |
|---|---|---|
| app_name | 11 | 64 |
| user_id | 10 | 64 |
| session_id | 25 | 64 |
| invocation_id | 38 | 64 |
| call_id | 32 | 64 |
| tool | 16 | 128 |
| sha256 | 64（定长） | CHAR(64) |
| payload | 82,540 | LONGBLOB |

## Elasticsearch

两个索引都是**可重建的派生索引**，权威数据在 MySQL。所以：不需要备份、
不需要迁移数据、坏了删掉重建。

### jelly-memory —— 替代 memory_fts

对应 `internal/memory/fts5.go`。它本来就是派生的：`AddSessionToMemory` 每轮
先 `DELETE` 掉该会话的旧行再重建，所以换后端不涉及数据迁移。**这是整套存储
里最容易换的一块。**

值得换的理由：现在用 FTS5 的 trigram 分词器，三字滑窗。它能做 CJK 子串匹配
（默认的 unicode61 会把一整句中文当成一个 token），代价是**查询必须 ≥3 个
字**，更短的走 LIKE 全扫。真分词没有这个下限。

字段对应关系直接照搬：FTS5 里除 `content` 外全是 `UNINDEXED`，这里对应
`keyword`；只有 `content` 走分词。`dynamic: strict` 是有意的——这个索引由
代码写入，字段集固定，多出来的字段一定是 bug，该报错而不是被静默索引。

### jelly-artifacts —— 大产物全文检索

服务 `search_result` 工具和 `GET /api/sessions/{id}/results/{ref}/search`。
现在这条路是把 payload 从 SQLite 读出来在进程里扫；产物实测已有 80KB，而
bench 里那个假 MCP 会造 6 万行日志。

正文**只索引不存储**（`store: false` + 从 `_source` 排除），命中靠 highlight
返回片段。这样 ES 不会变成 payload 的第二份副本：权威副本在
`tool_results.payload`，这里只是一个可重建的倒排索引。

文档 id 用 `app_name/user_id/session_id/invocation_id/call_id` 拼出来，和
MySQL 的唯一键同一把钥匙，重建时天然幂等。

> 一个已知边界：highlight 默认只分析正文前 1,000,000 字符
> （`index.highlight.max_analyzed_offset`）。超长产物要么调这个值，要么开
> `term_vector`。等真碰到再定——提前开 term_vector 是实打实的存储代价。

### 中文分词：两个版本选一个

| 文件 | 分词器 | 需要 |
|---|---|---|
| `*.json` | `ik_max_word` | 装 `analysis-ik` 插件 |
| `*.no-ik.json` | 内置 `cjk`（二元分词） | 无 |

IK 是真分词，明显强于现在的 trigram；`cjk` 二元分词不需要插件，效果介于
trigram 和 IK 之间。**先用 no-ik 版跑通，确认这条路有价值了再装插件。**

## 执行顺序

```bash
# MySQL
mysql -h HOST -u USER -p jelly < migrations/mysql/0001_init.sql
# ADK 那四张表由程序启动时 AutoMigrate 建，不用手动跑

# Elasticsearch
# Elasticsearch —— 没装 IK 插件就用 .no-ik.json
curl -XPUT "$ES/jelly-memory"    -H 'Content-Type: application/json' \
     -d @migrations/elastic/jelly-memory.no-ik.json
curl -XPUT "$ES/jelly-artifacts" -H 'Content-Type: application/json' \
     -d @migrations/elastic/jelly-artifacts.no-ik.json
```

四个 JSON 都是干净的 ES 请求体，可以直接 `-d @file`，不用先剥注释。

## 尚未验证的部分

Docker 起不来，所以下面这些是从源码和文档推出来的，**上线前要用真库过一遍**：

- ADK `AutoMigrate` 在 MySQL 上是否真能建出 events 表（那 16 字节余量）
- `LAST_INSERT_ID` 序列惯用法在并发 `Put` 下的实际表现
- 热路径延迟：`timeline.go` 的投影和 `taskapi.go` 的分页，本地文件 vs 网络
