# 存储迁移：SQLite → PostgreSQL

```
migrations/postgres/0001_init.sql    七张表 + 索引 + pg_trgm
```

一套 PG 解决全部：关系数据、中文检索、以后的向量。**不引 MySQL，也不引
Elasticsearch。**

## 为什么是 PG 而不是 MySQL

不是偏好，是这次迁移里风险最高的两项改造在 PG 上直接消失：

**① `seq` 分配不用重新设计。** `record.Put` 把序号分配写在 INSERT 的子查询
里，注释写明了它依赖什么：「One statement, so allocating the number and
writing the row cannot come apart」。MySQL 以 **error 1093** 拒绝这种语句
（INSERT 的子查询不许引用目标表），绕过去就得另建计数器表、改并发语义——
而 seq 是模型看到的 `e7` 句柄，错一次就指向别的证据。**PG 允许原语句。**

**② `tool_results` 主键不用改。** MySQL 里 TEXT 不加前缀长度不能进索引，
所以得换代理主键，连带 `ON CONFLICT` 的目标和唯一索引全部重新设计。PG 的
`text` 完全可索引，而且 PG 不按主键聚簇——二级索引带 6 字节 ctid，不是整个
主键。五列复合主键原样保留。

顺带还少三处：

| | MySQL | PG |
|---|---|---|
| `ON CONFLICT … DO UPDATE SET x=excluded.x` | 要改写成 `ON DUPLICATE KEY UPDATE` | **原样**（这本来就是 PG 语法，SQLite 抄的） |
| 大小写 | 默认 `utf8mb4_0900_ai_ci` 不区分大小写，`'A1b2'` 和 `'a1B2'` 会撞成同一个 call_id，所有标识列必须显式 `_bin` | 默认区分，和 SQLite 的 BINARY 一致，**没这个陷阱** |
| ADK 的 events 主键 | GORM 把 4 个无 size string 映射成 `varchar(191)`，4×191×4 = 3056，InnoDB 上限 3072，**只剩 16 字节** | 映射成 `text`，完全可索引，**问题不存在** |

## 为什么不引 Elasticsearch

原本打算把 memory 检索和大产物搜索放 ES。逐条看下来，两个都不值得：

**memory 检索**：现在用 FTS5 的 trigram 分词器。PG 的 `pg_trgm` 是同一个
东西，而且在 `contrib` 里、装 PG 就有。**行为和今天完全一致**，包括那个已知
限制（查询要 ≥3 个字）。ES 的 IK 是真分词、确实更好，但那是提升不是补齐，
而今天没人抱怨过检索质量。真要提升，路是加 `zhparser` / `pg_bigm` 扩展，
不必换数据库。

**大产物搜索**：`record.Search`（`internal/record/search.go:86`）是**正则 +
行号 + 上下文行**流式扫的。ES 的 highlight 做不到这几件——没有正则、没有
行号。换过去是退步。这条我一开始提反了。

**以后的向量**：`pgvector` 扩展，HNSW 索引。不用再引第三套。

## ADK 的四张表不在这里

`sessions` / `events` / `app_states` / `user_states` 由 ADK 自己的 GORM 模型
建（`internal/session/sqlite.go` → `database.AutoMigrate`）。**不要手写一份
DDL**：ADK 升级改了模型，AutoMigrate 会跟上，手写的那份只会静默漂移。

换 PG 就是换一个 dialector：

```go
database.NewSessionService(postgres.Open(dsn), &gorm.Config{...})
```

ADK 明确支持——`session/database/gorm_datatypes.go` 里 `case "postgres":
return "JSONB"`。

## 版本与扩展

- **PostgreSQL 13+**（`GENERATED AS IDENTITY`、`gin_trgm_ops`）
- `pg_trgm` —— contrib 自带，DDL 里第一行就 `CREATE EXTENSION`
- `pgvector` —— 只有做语义记忆那天才需要，现在不装

驱动用 `github.com/jackc/pgx/v5`（`stdlib` 包提供 `database/sql` 兼容），
GORM 侧用 `gorm.io/driver/postgres`。

连接串至少带：

```
?sslmode=... &timezone=UTC
```

时区必须钉死 UTC：现在所有时间都按 UTC 存，换机器时区不能让语义漂移。

## 和 SQLite 版的四处差异

这份 DDL 和原来的 schema 几乎一一对应。差异只有四处：

| 差异 | 原因 |
|---|---|
| `at` / `started_at` 等 → `timestamptz` | 原来存 RFC3339Nano 文本，而这个格式**砍掉末尾的零**：整秒的 `…T10:00:00Z` 在字符串比较里排在同一秒内的 `…T10:00:00.5Z` **之后**（`'.'` 的字节值小于 `'Z'`）。`ORDER BY at` 在秒内本来就是乱的。换真时间类型顺带修掉。 |
| `expired_at` → 可空 | 原来用空字符串表示未过期，因为 SQLite 那列是 NOT NULL。 |
| `ok` / `replayed` / `retrievable` → `boolean` | SQLite 用 INTEGER 存布尔。 |
| 新增 `tool_calls(session_id, invocation_id)` 索引 | 任务中心按一次运行取全部调用。本地文件上过滤一下无所谓，走网络值得让索引一次带出。 |

`memory_fts` 虚拟表变成普通表 `memory_index` + GIN trgm 索引，不算差异——
它是派生索引，删掉重建即可，没有数据要迁。

## 本地起一个（docker compose）

```bash
docker compose --profile db up -d
```

`0001_init.sql` 会在**首次启动**时自动跑（挂在
`/docker-entrypoint-initdb.d`）。healthcheck 除了 `pg_isready` 还查一下
`tool_results` 建出来没有——初始化脚本失败时容器仍然是 running，光看
`pg_isready` 发现不了。

```bash
docker compose --profile db ps          # 等 STATUS 变成 healthy
psql "postgresql://jelly:jelly-dev@localhost:5432/jelly"
```

改了 schema 要重来（初始化脚本只在数据目录为空时执行）：

```bash
docker compose --profile db down -v     # -v 才会删掉命名卷
docker compose --profile db up -d
```

口令是开发用的默认值，可以用 `PGUSER` / `PGPASSWORD` 覆盖。**线上走你们
自己的实例和密钥管理，别用这套。**

## 在已有实例上执行

```bash
psql "$DSN" -f migrations/postgres/0001_init.sql
# ADK 那四张表由程序启动时 AutoMigrate 建，不用手动跑
```

**重跑会报 `42P07 relation already exists`，这是故意的。** `CREATE TABLE`
没写 `IF NOT EXISTS`：那个写法对一张已经存在、但定义已经变了的表**什么都
不做**，而这正是 record / metrics / schedule 三个包分别踩过一次的坑
（见它们各自 migrate 函数的注释）。宁可在你面前失败，也不要静默地让 schema
和代码对不上。

用 DBeaver 之类的客户端跑时尤其注意：报了 already exists 就说明**上一次已经
成功了**，去看表在不在，而不是反复重试。要重来就先 `DROP SCHEMA public
CASCADE; CREATE SCHEMA public;`。

`CREATE EXTENSION pg_trgm` 需要建库权限；托管实例上通常要用管理员账号跑
这一行，或者先让 DBA 装好。

## 已在真库上验过（PostgreSQL 16.6）

三条原本推断出来的结论，都在 `172.16.5.128:5432/jelly` 上跑过了：

**① `0001_init.sql` 能跑通。** 7 张表、13 个索引、7 个主键、`pg_trgm 1.6`
全部就位。

**② ADK 的 `AutoMigrate` 在 PG 上没有 MySQL 那个隐患。** 四张表都建出来，
**每个字符串列都是 `text`** —— 没有 `varchar(191)` 的截断，也没有那个
「4×191×4 = 3056，距 InnoDB 上限只剩 16 字节」的问题。events 的主键是
`btree (id, app_name, user_id, session_id)`，四列 text，PG 完全不在意。
state / content / 各种 metadata 用的是 `jsonb`（MySQL 上会是 LONGTEXT）。

复现：`internal/session/pgprobe_test.go`，设 `JELLY_PG_DSN` 才跑。留着它是
因为每次升 ADK 都值得重问一遍这个问题。

**③ `record.Put` 那条 UPSERT 一字不改就能跑。** 这是整个 PG 方案的前提，
实测：

| 场景 | 结果 |
|---|---|
| 连续三次交付 | seq = 1, 2, 3 |
| 同一 call_id 重放 | seq 不变（仍是 2），内容更新 |
| 两条交付抢同一个 seq | 唯一索引拒绝写入，不会共用句柄 |

MySQL 会以 error 1093 拒绝这条语句（INSERT 的子查询引用了目标表）。

**附带测出来的：`pg_trgm` 对中文是有效的。** `show_trgm('内存使用率')` 给出
6 个哈希三元组（多字节字符的三元组以哈希形式存，这是正常的）。索引可用，
但**小表上 planner 会正确地选顺序扫**：

| 行数 | 执行计划 | 耗时 |
|---|---|---|
| 5,000 | Seq Scan | —— 表只有几页，顺序扫本来就更快 |
| 200,000 | Bitmap Index Scan | 1.26 ms |

所以短期内看到 Seq Scan 不是索引坏了，是数据量还没到。

## 句柄生命周期（已解决）

七个 store 里曾有四个是**每次调用现开一个句柄再关掉**（`open()` → `defer
db.Close()`）：`internal/task`、`internal/session/list`、
`internal/memory/purge`、`internal/schedule`。

在 SQLite 上这是开个文件，可以忽略。在 PG 上这是一次 TCP + 认证握手。
对着 `172.16.5.128:5432` 实测（局域网，和真实部署形态一致）：

| | 每次 `SELECT 1` |
|---|---|
| 每次调用现开句柄 | **9.337 ms** |
| 复用已有句柄 | **579 µs** |
| | **16 倍** |

`session.ListPage` 在每次会话页加载时付一次；`handleDeleteSessions` 连着调
三个清理函数就是三次握手。

现在这四个 store 都改成接收 `*storage.DB`，句柄由 `engine.StateDB()` 持有，
整个进程一个。守卫测试盯着 `defer db.Close()` 不让它长回来。

顺带修掉的：

- 这些 opener 每次调用都跑一遍 `CREATE TABLE IF NOT EXISTS`（`schedule` 还
  多跑两次 migrate 探测）。对已存在的表是空操作，但仍然是往返。现在各包
  暴露 `EnsureSchema`，开句柄时统一跑一次。
- `schedule.open()` 调的是 `session.DefaultDBPath()`，**忽略配置的路径** ——
  一个把状态库挪了位置的部署，排程历史会被悄悄写到默认位置。接收共享句柄
  从结构上消除了这个问题。

`postgresConnLimit` 现在仍是 4：`record` / `metrics` / `memory-fts` 三个
store 还各自持有自己的句柄，所以进程仍有四个池。把它们也收到
`engine.StateDB()` 之后这个数要重定。

## 还没验的一项

**热路径延迟**：`timeline.go` 的投影和 `taskapi.go` 的分页。这一条要等
store 真正接到 PG 之后才测得了 —— 现在代码还在读 SQLite。
