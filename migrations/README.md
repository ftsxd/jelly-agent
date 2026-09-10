# 存储迁移：SQLite → PostgreSQL

```
migrations/postgres/0001_init.sql    七张表 + 索引 + pg_trgm
```

一套 PG 解决全部：关系数据、中文检索、以后的向量。**不引 MySQL，也不引
Elasticsearch。**

## 怎么确认这套东西是对的

跑测试。PostgreSQL 相关的是 opt-in 的，设 `JELLY_PG_DSN` 才跑：

```bash
go test ./...                                   # SQLite 侧，无需任何外部依赖
JELLY_PG_DSN=postgres://… go test ./internal/... # 同一批测试，跑在 PG 上
go test ./internal/server/ ./internal/task/ -race
cd web && npx vitest run && npx vite build
```

值得知道的几条：

| 测试 | 它守着什么 |
|---|---|
| `engine.TestSQLiteAndPostgresSchemasAgree` | 两份 schema 定义不会漂开，表清单从 `migrate.Tables` 推导 |
| `engine.TestEveryStoreWorksAgainstPostgres` | 每个 store 在 PG 上的读写往返 |
| `server.TestHandlersWorkAgainstPostgres` | handler 层，投影和分页自己拼的 SQL |
| `server.TestTheTaskListDoesNotReloadUnchangedSessions` | 任务列表的 N+1 不会回来 |
| `server.TestHotPathLatency` | 三条热路径的实际耗时，打印出来 |
| `migrate.TestMigrateSQLiteToPostgres` | 整条升级路径，含幂等与审计历史 |
| `engine.TestOpeningWithNoConfiguredDirectoryLeavesTheDefaultOneAlone` | 没配目录的进程不动默认目录里的文件 |
| `storage.TestDialectKnowledgeLivesOnlyInThisPackage` | 方言代码不会长回别的包 |

PG 测试共用一个开发库，靠 `pg_advisory_lock` 串行化（`storage.LockExclusively`）
—— 不是靠记得加 `-p 1`。

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
东西，而且在 `contrib` 里、装 PG 就有。ES 的 IK 是真分词、确实更好，但那是
提升不是补齐。真要提升，路是加 `zhparser` / `pg_bigm` 扩展，不必换数据库。

**但两边不是同一条查询**（`internal/memory/fts5.go` 的 `matching`）：

| | SQLite | PostgreSQL |
|---|---|---|
| 索引 | FTS5 虚拟表，trigram 分词器 | 普通表 + `GIN (content gin_trgm_ops)` |
| 命中 | `content MATCH ?` | `content ILIKE ?` |
| 排序 | FTS5 的 `rank` | `similarity(content, ?) DESC` |
| <3 字 | 必须绕开 —— MATCH 直接拒 | 同一条查询照常答，只是索引加速不了 |

这个差异**没有藏在共同抽象后面**，代码里显式分支：占位符和 `strftime` 是同
一条查询的两种拼写，调用方不该看见；全文检索是两套实现，假装成一套会让人
不知道跑的是哪个。

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
| `schedule_runs.started_at` / `finished_at`、`tool_decls.updated_at` → `timestamptz` | 这几处 Go 侧本来就传 `time.Time`、也扫回 `time.Time`，真时间类型是对的。 |
| `tool_results.at` / `tool_calls.at` / `task_runs.at` **保持 `text`** | 见下面「撤回的一条」。 |
| `ok` / `replayed` / `retrievable` → `boolean` | SQLite 用 INTEGER 存布尔。 |
| 新增 `tool_calls(session_id, invocation_id)` 索引 | 任务中心按一次运行取全部调用。本地文件上过滤一下无所谓，走网络值得让索引一次带出。 |

`memory_fts` 在 PG 上是普通表 + GIN trgm 索引，**表名保持 `memory_fts`**——
名字是这个存储的名字不是技术的名字，而查询里的表名是写死的。改名要迁移已有
的 SQLite 库，换不来任何东西。

### 撤回的一条：时间列没有换成 timestamptz

前一版这份 README 说 `at` 换成 `timestamptz`「顺带修掉」了那个排序 bug。
**做不到，已撤回。**

Go 侧存的是 RFC3339Nano 文本、读回来也扫进 `string`
（`internal/record/store.go` 的 `Put` 与 `read`）。换成真时间类型要同时改读写
两端，还要迁移已有 SQLite 库里的数据 —— 那是一次独立改动，混进方言接入里，
出问题时分不清是哪一边。

**排序 bug 仍然存在，两个方言都有**：RFC3339Nano 砍掉末尾的零，整秒的
`…T10:00:00Z` 在字符串比较里排在同一秒内的 `…T10:00:00.5Z` **之后**
（`'.'` 的字节值小于 `'Z'`），所以 `ORDER BY at` 和 `idx_tool_results_at`
在秒内是乱的。

这条是怎么被发现漏改的，值得记一笔：端到端测试第一版没有校验时间往返，
于是一个声明成 `timestamptz`、而代码按字符串读写的列**照样通过**——值以
数据库自己的格式回来，没人看。补上断言之后才暴露。

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

## 句柄分配：从乐观重试改成计数器（已在真库验过）

原来的分配写在 INSERT 内部：`COALESCE(MAX(seq),0)+1`。它读和写之间不持有
任何锁，所以 N 条交付同时落地时全都读到同一个最大值。在这台 PG 上实测：

| 并发工具数 | 第几次才成功 | 永久丢失的交付 |
|---|---|---|
| 2 | 1, 2 | 0 |
| 4 | 1, 2, 3, 4 | 0 |
| **6** | 1, 2, 3, 4 | **2** |
| **8** | 1, 2, 3, 4 | **4** |

规律是：**N 个并发写入，最倒霉那个正好需要第 N 次**。而重试上限是 4，
`max_tools` 是 64。

调大重试次数不是修法。而且重试**本来也救不了**——分配和写入在同一个事务里，
回滚会把计数器一起退回，下一次重试拿到的还是同一个号。

改成 `tool_result_seq`：每会话一行，`UPDATE … RETURNING` 一步完成自增和读取。
UPDATE 拿行锁，并发写入排队几微秒，各自拿到不同的号。同一个实验：

| 并发工具数 | 拿到的句柄 | 丢失 | 重复 |
|---|---|---|---|
| 2 / 4 / 6 / 8 | 1..N，连续 | 0 | 0 |
| 12 | 1..12 | 0 | 0 |
| 24 | 1..24 | 0 | 0 |

代价是**契约变了**：

```
原来：连续          e1, e2, e3 …
现在：单调、不复用，但不连续
```

重放同一个 call 会花掉一个号而那一行不用（它保留模型已经看到的旧句柄），
于是出现空洞。这个弱一些的承诺和原来那个不同的地方在于：**它是真的做得到的。**

`record.Delete` 在同一事务里连计数器行一起删——留着的话，一是这一行仍然
写着 session id，「清除全部痕迹」就没做到；二是这个 session id 万一回来，
第一条交付会从被删掉那次对话停下的地方接着编号。

旧库的回填按每个 scope 的 `MAX(seq)`，**不是 `COUNT(*)`**。两者只在号码连续
时相等，而现在不连续了：一个号码是 1,2,4 的会话有三行，按 COUNT 播种会说 3、
下一个发 4，而 4 已经被唯一索引占着——库能干净打开，然后这个会话的下一条
交付就失败。

## 两份 schema 不会悄悄漂开

这份迁移文件和各个 store 里的 DDL 常量是**两份独立定义**。给一边加一列、
忘了另一边，运行时不会有人抱怨 —— `storage.ApplySchema` 只检查表在不在，
不检查里面有什么，而且它也做不到更多：PG 部署从来不跑 SQLite 的 DDL。

只有一个同时看得到两个库的测试能比。
`internal/engine/schemadrift_test.go` 就是它：在临时 SQLite 上跑一遍所有
store 的建表，再对着已经迁移过的 PG，逐表比列名。设 `JELLY_PG_DSN` 才跑。

两个方向都验过：

```
tool_results.brand_new 只在 SQLite 上有 —— migrations/postgres/0001_init.sql 少了它
task_runs.only_in_pg   只在 PostgreSQL 上有 —— 某个 store 的 DDL 少了它
```

**只比列名，不比类型。** `INTEGER` 对 `boolean`、`TEXT` 对 `text` 是有意的
（见上面那张差异表）；而「一边有、另一边没有」从来不是有意的。

ADK 那四张表不在比较范围内：两边都由 GORM AutoMigrate 从同一套模型建，
不会像两份手写定义那样漂。

## 把现有数据搬过去

```bash
# 1. 目标库建表
psql "$DSN" -f migrations/postgres/0001_init.sql

# 2. 看看会搬多少，不写任何东西
jelly migrate --from ~/.jelly-agent/state.db --dry-run

# 3. 停掉服务，再搬
jelly migrate --from ~/.jelly-agent/state.db
```

**服务必须先停。** 命令不加锁，边搬边写会漏掉搬运过程中新写入的行——不过它
搬完会核对行数，那种情况会被报出来，不会静悄悄地少。

**可以重跑。** 每条插入都是 `ON CONFLICT DO NOTHING`，中途断了直接再来一次。

**默认数据库跟着配置文件走。** 没有配 `storage.dsn` 时，状态库是配置文件旁边的
`state.db`——和 `tools.metadata_dir` 的默认值同一个规则。这两个以前不一致
（元数据目录跟配置走，数据库硬编码在 `~/.jelly-agent`），于是同一份配置的两半
指向不同地方。

**源库有目标库没有的列、且那些列里有值时，它会拒绝并报出列名**，而不是照搬其余的。迁移是那种
没人会回头核对的操作——源库马上就不再被读了，悄悄丢掉的值就真的没了。两份
schema 定义差一个提交是常有的事，修法通常是往 `0001_init.sql` 里加一行。
确实不要那些值就用 `--allow-dropping-columns`。

全空的列不算——`tool_results` 带着一个没人再写的 `label`，每行都是空串，
为它拦下每一次真实升级没有道理。会在输出里说一句，但不会停。

`--dry-run` 在目标库还没有 ADK 那四张表时也能报数：它就是给"要不要迁"这个
决定用的，而那四张表要等服务启动才建。

**回滚是把 `storage.dsn` 改回去**，源库自始至终没被改动。但注意时间窗口：
搬完之后服务在新库上写下的东西，回滚时不会回到源库。

### 搬什么、不搬什么

十一张表：ADK 的 `sessions` / `events` / `app_states` / `user_states`，加上
`tool_results` / `tool_result_seq` / `tool_calls` / `task_runs` /
`schedule_runs` / `tool_decls` / `tool_decl_log`。顺序有讲究——events 引用
sessions。

**`memory_fts` 不搬。** 它是从 events 重建的派生索引，每个会话的下一轮对话
会自动重建，搬过去只是把马上要被覆盖的旧行搬一遍。

`tool_result_seq` 搬是为了**精确**而不是为了**正确**：目标库没有它的话，
`record.Open` 会按每个会话的 `MAX(seq)` 重新播种，句柄照样不会撞，只是丢掉
重放烧掉的号、空洞合上。

### 它为什么是 Go 命令而不是一段 SQL

三个理由，都写在 `internal/migrate` 的包注释里：

- **类型差异是真的。** SQLite 用 0/1 存布尔、用文本存时间；PG 两样都有真类型。
  dump-and-load 得把同样的转换在 SQL 里再写一遍。
- **schema 已经有两份定义了**（各 store 的 DDL、这个迁移文件）。一个自带列
  清单的搬运器会是第三份，而且会和两边都漂。这个搬运器**从目标库实际读列名
  和类型**。
- **核对得和搬运在一起。** 没法核对的迁移就是没法信任的迁移。

### 实测

拿一个真实的 2MB `state.db` 加 9 条 `console.yaml` 声明，端到端走完整条
升级路径：

```
1. SQLite 上启动一次   → console.yaml 导入 tool_decls（9 条，带 import: 署名）
                          原文件改名 console.yaml.imported
2. psql -f 0001_init.sql
3. migrate --dry-run    → 报出会搬多少，一行都不写（ADK 的四张表也不建）
4. migrate              → 115 行，行数核对通过
5. 再跑一次 migrate      → 复制 0 行，已存在 115 行
6. 在 PG 上启动服务      → 首页 200、未鉴权端点 401、日志零错误
                          9 条声明与审计历史都在
```

数据是 2 个会话、16 条事件、13 条产物（含一条 82,540 字节的）、66 条调用
记录。

## 热路径延迟（已实测）

`internal/server/pglatency_test.go`，设 `JELLY_PG_DSN` 才跑。每个库灌同样的
数据：24 个会话 × 30 条事件，其中 4 条带产物；每项取 9 次的中位数。对着
`172.16.5.128:5432`（局域网）。

| | 会话时间线 | 任务列表 | 会话列表 |
|---|---|---|---|
| SQLite | 384 µs | 10.6 ms | 2.4 ms |
| PostgreSQL | **19.7 ms** | **642 ms** | **9.3 ms** |
| 倍数 | 51× | 61× | 4× |

**结论：能用，但任务列表那条要盯着。** 时间线和会话列表都在人感知不到的范围
内；任务列表 642ms 是一个人会觉得卡的数字。

### 量出来的四个问题（都已修）

第一版数字是 872ms / 1531ms / 1207ms。四个问题都和 PG 无关，只是被 PG 放大
——在 SQLite 上同样存在，便宜到看不见。

**① 每个请求重建一次 ADK 会话服务。** 六个 handler 都调
`NewSessionService()`，而它每次开连接 + 跑 `AutoMigrate`：SQLite 857µs 一次，
PG **733ms** 一次，占时间线总耗时的 85%。改成 `sync.Once`。

**② 会话列表每行一次完整会话加载。** `sessionPreview` 为了一行预览把整个
会话的事件读出来。首条用户消息创建后不再变，所以能缓存
（`internal/server/preview.go`）。

**③ 任务列表每会话一次完整加载 + 一次链接查询。** 这两条都改成按页批量：

- 链接：`task.OfSessions(db, ids)`，一条查询一页。
- 事件：`session.EventsOf(ctx, db, app, user, ids)`，一条查询一页。
  ADK 的服务没有批量接口，但它把 `content` / `actions` 存成自己公开类型的
  普通 JSON（见 `database.createEventFromStorageEvent`），所以直接读表是
  可行的，也是唯一能让往返次数不随会话数增长的办法。

  读别人的表，风险是对方换写法。`TestBatchReadMatchesWhatADKReadsBack`
  通过 ADK 写入、两边分别读回来逐字段比对（含顺序），升 ADK 时会红。
  某一列解不开时不会静默降级：报出 session、event、列名，并把**那个会话**
  整个排除在外——带着残缺内容显示，会让它看起来像一个什么都没做的任务。
  `handleTasks` 现在**根本不打开会话服务**。

  投影缓存还在，但它降级成纯加速：**冷缓存下的往返次数也已经是常数**，
  这一点由 `TestTheTaskListNeverReadsSessionsOneAtATime` 在 4/16/40 个会话
  上断言完整 `Get` 次数为 0，且全程不预热。

**④ ADK 的表上一个索引都没有。** 这是最坏路径的真正大头，而且不在本仓库
写的代码里：ADK 的模型只声明了复合主键，`events` 是
`(id, app_name, user_id, session_id)`、id 在最前，所以「这个会话的事件」没有
可用前缀；`sessions` 上没有 `update_time` 的索引，「按最近排序」要排整张表。

650 个会话时，任务扫描翻 15 页要 **SQLite 3.8 秒 / PG 3.1 秒**，几乎全在这里。
`session.EnsureIndexes` 补两条索引，在**两个**地方各跑一次：

- `AutoMigrate` 之后——全新安装，那时表才刚建出来；
- 打开共享句柄时（`EnsureSchema`）——**升级中的部署走的是这条**。它的表来自
  旧版本，`AutoMigrate` 不会跑；而任务列表已经不打开会话服务了，所以一个人
  启动新二进制、直接打开任务页，只经过这条路径。

`CREATE INDEX IF NOT EXISTS` 跑两遍是免费的，表还不存在时跳过。加索引是增量
的，不像手写一份 ADK 的建表语句那样会随它改模型而漂。

| | 加索引前 | 加索引后 |
|---|---|---|
| SQLite，650 会话翻 15 页 | 3822 ms | **2 ms** |
| PostgreSQL，同上 | 3083 ms | **69 ms** |

### 现在的数字

24 个会话 × 30 条事件。**冷**是进程刚起、缓存为空的第一个请求；**热**是之后
的中位数。

| | 任务列表(冷) | 过滤不中(冷) | 任务列表(热) | 会话时间线 | 会话列表 |
|---|---|---|---|---|---|
| SQLite | 4.7 ms | 492 µs | 259 µs | 309 µs | 160 µs |
| PostgreSQL | **41.7 ms** | **3.7 ms** | 3.2 ms | 2.4 ms | 1.6 ms |

最坏路径按真实规模单独测（620 个会话，超过 `taskScanMax = 600` 的上限，
过滤条件匹配不到，冷缓存）：

| | 冷 | 热 |
|---|---|---|
| SQLite | 19 ms | 3 ms |
| PostgreSQL | **191 ms** | 101 ms |

对比最初：任务列表 1531ms → 冷 41.7ms，最坏路径 12 秒（推算）→ 冷 191ms。

**这些是冷路径的数字，不是预热之后的。** 早先一版的延迟测试用中位数报告，
而取中位数前会先跑一次预热，所以报出来的全是热缓存结果——那让一个缓存看起来
像是修复。现在冷、热分开报，冷的那一列只测第一个请求。
