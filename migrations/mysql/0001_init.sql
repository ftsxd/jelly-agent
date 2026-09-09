-- jelly-agent · MySQL 初始 schema
--
-- 对应 SQLite 版本：internal/{record,metrics,task,schedule}/*.go 里的 schema 常量。
-- ADK 自己的四张表（sessions / events / app_states / user_states）不在这里，
-- 见本目录 README 的说明——它们由 GORM AutoMigrate 建，手写一份必然漂移。
--
-- 要求 MySQL 8.0+（LAST_INSERT_ID 序列惯用法、DATETIME(6)、utf8mb4 默认）。

-- ─────────────────────────────────────────────────────────────────────────
-- 字符集与排序规则：两条不能省的规定
--
-- 1) 标识列一律 ascii + ascii_bin。
--    session_id / invocation_id / call_id / sha256 全是 UUID、十六进制或
--    生成的短 id，永远不含非 ASCII。用 ascii 让每字符 1 字节而不是 4 字节，
--    这是主键能塞进 InnoDB 3072 字节上限的关键。
--
-- 2) _bin 不是为了省空间，是为了正确。
--    SQLite 的 TEXT 默认按 BINARY 比较（区分大小写）；MySQL 默认的
--    utf8mb4_0900_ai_ci 不区分大小写也不区分重音。照搬默认值会让两个
--    本来不同的 call_id 撞成一个 —— 'A1b2' 与 'a1B2' 在 MySQL 默认排序
--    规则下相等。所有当主键/唯一键用的标识列必须显式 _bin。
--
-- 文本内容列（工具名之外的自由文本、错误信息、args）用 utf8mb4，
-- 它们要装中文。
-- ─────────────────────────────────────────────────────────────────────────


-- ═══ tool_results ════════════════════════════════════════════════════════
-- 工具产物。模型看到的 e1/e2 句柄就是这里的 seq。
--
-- 和 SQLite 版的三处结构差异，都是被 MySQL 逼出来的：
--
-- ① 主键换成代理键。SQLite 版的主键是 5 个 TEXT 列。MySQL 里 TEXT 不加前缀
--    长度根本不能进索引；换成有界 VARCHAR 后能建，但 InnoDB 是聚簇索引，
--    每条二级索引都要背着这个宽键。改用 BIGINT 自增做主键、原来的五列做
--    唯一键，二级索引只背 8 字节。
--
-- ② payload 用 LONGBLOB。MySQL 的 BLOB 上限 64KB，而现网实测已经有 82,540
--    字节的单条产物。别忘了同时调 max_allowed_packet（见 README）。
--
-- ③ at 用 DATETIME(6) 而不是照搬字符串。SQLite 版存的是 RFC3339Nano 文本，
--    而这个格式会砍掉末尾的零：整秒的 "…T10:00:00Z" 在字符串比较里排在
--    同一秒内的 "…T10:00:00.5Z" 之后（'.' < 'Z'）。ORDER BY at 因此在秒内
--    是乱的。换成真正的时间类型顺带修掉它。
CREATE TABLE tool_results (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,

  app_name      VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  user_id       VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  session_id    VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  invocation_id VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  call_id       VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,

  -- 会话内的交付序号，模型看到的 e{seq}。由 tool_result_seq 分配，
  -- 不由调用方给：进程内计数器一重启就会把下一轮的编号指到别的证据上，
  -- 这是一个持久句柄唯一不能出的错。
  seq           BIGINT UNSIGNED NOT NULL,

  tool          VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  server        VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  at            DATETIME(6)  NOT NULL,
  bytes         BIGINT       NOT NULL,
  sha256        CHAR(64)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  upstream      VARCHAR(32)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'unknown',

  -- 非 NULL 表示保留期已经把正文清掉了。行留着，句柄才继续解析得出来——
  -- 见 retention.go：这比省下的那一百字节重要得多。
  -- SQLite 版用空字符串表示未过期，这里用 NULL，因为 MySQL 有真正的可空时间。
  expired_at    DATETIME(6)  NULL DEFAULT NULL,

  payload       LONGBLOB     NOT NULL,

  PRIMARY KEY (id),
  UNIQUE KEY uk_tool_results_call (app_name, user_id, session_id, invocation_id, call_id),
  UNIQUE KEY uk_tool_results_seq  (app_name, user_id, session_id, seq),
  KEY idx_tool_results_session (session_id),
  KEY idx_tool_results_at      (at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;


-- ═══ tool_result_seq ═════════════════════════════════════════════════════
-- seq 的分配器。这张表在 SQLite 版里不存在，是 MySQL 逼出来的。
--
-- SQLite 版把分配写在插入语句内部：
--     INSERT INTO tool_results (...,seq,...)
--     VALUES (?,…,(SELECT COALESCE(MAX(seq),0)+1 FROM tool_results WHERE …),…)
-- 注释里写明了它依赖什么：「One statement, so allocating the number and
-- writing the row cannot come apart.」
--
-- MySQL 不允许在 INSERT 的子查询里引用插入目标表（error 1093）。套一层
-- 派生表能骗过语法检查，但那正好丢掉它唯一想要的原子性。
--
-- 所以分配挪到这里，用 MySQL 的序列惯用法，仍然是一条语句、仍然原子：
--     INSERT INTO tool_result_seq (app_name,user_id,session_id,next_seq)
--     VALUES (?,?,?,LAST_INSERT_ID(1))
--     ON DUPLICATE KEY UPDATE next_seq = LAST_INSERT_ID(next_seq + 1);
--     SELECT LAST_INSERT_ID();
-- 两个分支都调 LAST_INSERT_ID(expr)，所以首次插入和后续递增拿到的都是本次
-- 分配到的值，且是连接级的，不受其他连接干扰。
CREATE TABLE tool_result_seq (
  app_name   VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  user_id    VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  session_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  next_seq   BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (app_name, user_id, session_id)
) ENGINE=InnoDB DEFAULT CHARSET=ascii COLLATE=ascii_bin;


-- ═══ tool_calls ══════════════════════════════════════════════════════════
-- 每次工具调用的耗时与结果。只写不改，查询按 (session, invocation) 或按工具。
CREATE TABLE tool_calls (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  at            DATETIME(6)  NOT NULL,
  session_id    VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  invocation_id VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  agent         VARCHAR(128) NOT NULL DEFAULT '',
  call_id       VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  tool          VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  args          TEXT         NOT NULL,
  duration_ms   BIGINT       NOT NULL DEFAULT 0,
  ok            TINYINT(1)   NOT NULL,
  err_kind      VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  err           TEXT         NOT NULL,
  result_bytes  BIGINT       NOT NULL DEFAULT 0,
  evidence_id   VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  replayed      TINYINT(1)   NOT NULL DEFAULT 0,
  retrievable   TINYINT(1)   NOT NULL DEFAULT 0,

  PRIMARY KEY (id),
  KEY idx_tool_calls_at      (at),
  KEY idx_tool_calls_session (session_id),
  KEY idx_tool_calls_tool    (tool),
  -- SQLite 版没有这条。任务中心按 (session, invocation) 取一次运行的全部
  -- 调用（metrics.ByInvocation），现在只能走 idx_tool_calls_session 再过滤。
  -- 一次运行的行数不多，本地文件上无所谓；走网络就值得让索引把它一次带出。
  KEY idx_tool_calls_run     (session_id, invocation_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;


-- ═══ task_runs ═══════════════════════════════════════════════════════════
-- 一次运行属于哪个任务。任务 id 就是 session_id + "/" + invocation_id，
-- 所以这张表本质是「续问归到哪个任务」的映射。
CREATE TABLE task_runs (
  task_id       VARCHAR(160) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  session_id    VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  invocation_id VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  at            DATETIME(6)  NOT NULL,

  PRIMARY KEY (session_id, invocation_id),
  KEY idx_task_runs_task (task_id)
) ENGINE=InnoDB DEFAULT CHARSET=ascii COLLATE=ascii_bin;


-- ═══ schedule_runs ═══════════════════════════════════════════════════════
CREATE TABLE schedule_runs (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  task          VARCHAR(128) NOT NULL,
  started_at    DATETIME(6)  NOT NULL,
  finished_at   DATETIME(6)  NOT NULL,
  status        VARCHAR(32)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  output        MEDIUMTEXT   NULL,
  error         TEXT         NULL,

  -- 这两列把一次定时运行和它的步骤、产物连起来。没有它们，排程运行在控制台
  -- 里就只是一个状态加一坨文本，够不到背后的执行过程——别的表全都是按
  -- 这一对键的。
  session_id    VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  invocation_id VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',

  PRIMARY KEY (id),
  KEY idx_schedule_runs_task (task, started_at),
  KEY idx_schedule_runs_run  (session_id, invocation_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;


-- ═══ tool_decls ══════════════════════════════════════════════════════════
-- 控制台改的工具元数据。这张表是这次迁移的起因。
--
-- 之前它是一个文件（~/.jelly-agent/tools/console.yaml），界面每次保存都
-- 整文件读-改-写。后果在 2026-09-08 兑现了：一次保存把刚写进去的 suites
-- 清空，而且没有任何历史可以查回来。多进程下还会互相丢更新——进程内的
-- 那把锁跨不了进程。
--
-- 列可以为 NULL，NULL 就是「本文件不声明这一项，沿用上游」。这样 patch
-- 语义变成结构性的，而不是靠代码里的 if x != nil 维持。
--
-- 仓库里 configs/tools/*.yaml 仍然是文件：那是人手写、要 review、要进
-- git 的基座。这张表只装界面改的覆盖层。
CREATE TABLE tool_decls (
  server        VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  name          VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,

  description   TEXT         NULL,
  use_cases     JSON         NULL,
  examples      JSON         NULL,
  anti_examples JSON         NULL,
  suites        JSON         NULL,
  produces      VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NULL,
  side_effect   VARCHAR(32)  CHARACTER SET ascii COLLATE ascii_bin NULL,

  updated_at    DATETIME(6)  NOT NULL,
  updated_by    VARCHAR(128) NOT NULL DEFAULT '',

  PRIMARY KEY (server, name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;


-- ═══ tool_decl_log ═══════════════════════════════════════════════════════
-- 追加写的改动历史，兼版本号。
--
-- 两个用途，一个表：
--   1. 审计——今天丢掉的正是这个。谁在什么时候把 suites 改成了什么。
--   2. 失效通知——各进程的注册表是内存态，轮询 SELECT MAX(id) 这一个整数
--      就知道要不要 reload。这同时替掉了 toolreg.FileSource.Watch 那套
--      比对文件 mtime 的做法，而 mtime 本来就跨不了进程。
CREATE TABLE tool_decl_log (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  server     VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  name       VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  -- upsert | delete
  op         VARCHAR(16)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  -- 这次写入之后该工具的完整声明，NULL 表示删除。存快照而不是 diff：
  -- 要回溯「当时到底是什么」，快照一行就够，diff 要从头重放。
  snapshot   JSON         NULL,
  changed_at DATETIME(6)  NOT NULL,
  changed_by VARCHAR(128) NOT NULL DEFAULT '',

  PRIMARY KEY (id),
  KEY idx_tool_decl_log_tool (server, name, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
