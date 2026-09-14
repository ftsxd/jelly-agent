# Jelly Agent 接入 OpenViking：只读运维知识库

状态：**设计方案，已完成上游源码核验；实现已于 2026-09-14 整体撤回，当前代码库中没有任何 OpenViking 相关代码。** 上游源码核验完成于 2026-09-11，2.7 的实例实测结论仍然有效。更新日期：2026-09-14。

> **2026-09-14 决定：不接入，撤回全部实现代码。**
>
> 阶段 A/B 曾按本文实现过一遍（客户端、召回与预算装配、来源登记、两个只读工具、瞬态注入、控制台页面、迁移与 compose profile），随后整体撤回。
>
> 撤回的理由不是实现有问题，是**投入产出没有被证明**：到撤回时语料仍为空，§9.2 的 30 题闸门一次都没跑过，也就没有任何数字支持「语义召回比一个目录 + 现有 L2 检索更值」。而代价是明确的——多一个单实例单写者、无高可用的服务要运维备份，embedding 要自建一台机器，VLM 按导入量计费且 reindex 会重复产生，文档正文要出网。
>
> 更便宜的替代方案已经在仓库里：**Skills**（Markdown 能力包、清单注入、`use_skill` 按需加载），零新增基础设施、不出网、不花钱。分界线在**谁来找文档**：人知道该看哪篇，Skills 就够；要 Agent 从症状描述里自己找出对的材料，才需要语义检索。
>
> **重新评估的触发点**：手册规模超出清单注入能承受的量，或准确率目标被证明卡在「没找到对的材料」而不是别的环节上。届时先跑 §9.2 的闸门（30 个真实问题、Top-5 ≥ 85%，并与纯关键词基线对比），达标再谈接入。
>
> 本文其余部分是上游核验与实例实测的结论，**不随实现撤回而失效**，重新评估时直接可用。

## 1. 结论与本轮范围

**本轮只做一件事：把运维知识接入 OpenViking，只读。** Skills 与长期记忆本轮不迁移，第 9.5 节写明各自的前置条件与重新评估触发点。「知识、记忆、Skills 统一由 OpenViking 管理」仍是终态，但本文不为后两者排期。

三条判断依据如下。

**知识域是 Jelly 的真空白，上游能力扎实。** Jelly 目前没有任何语义检索。L2 会话检索只有 trigram 匹配，`internal/memory/fts5.go:148-154` 的注释自己写明「真正的分词是改进而非修复」。运维手册、架构文档、已确认复盘案例目前完全不在 Agent 视野内，而准确率目标恰恰依赖这些资料。上游的导入管线与 L0/L1 分层摘要正是自建成本最高、收益最直接的部分。失败模式也温和：召回不到只是退化，不污染既有数据；原始文档仍以 Git 与文档系统为事实源，退出成本低。

**Skills 暂不迁移，理由是上游共享根没有任何授权控制。** `viking://agent/skills` 是账号级共享根，account 内任何 USER 角色都可读、可写、可删（`_access.py:1023-1026`、`405-413`；删除 `routers/skills.py:842-843`），而 ACL 被硬限制为只作用于 `viking://resources`（`storage/acl.py:227-235`）。上游 `PUT /skills/{name}` 是破坏性覆盖，成功后即删除备份，没有版本仓库（`routers/skills.py:749-813`）。OVPack 的 backup 公共范围也不含这个根（`storage/ovpack/policy.py:21-24`）。要安全使用，必须在 Jelly 侧自建发布记录并按 `revision` 锁定，而那部分工作量已占大头，OpenViking 退化成一个 Git 就能替代的文件存储。

**长期记忆的卡点在 Jelly 自身，不在 OpenViking。** `internal/engine/engine.go:70` 的 `UserID = "local-user"` 是编译期常量，约三十处引用，含 `record.Scope`。`internal/platform/platform.go:16,22` 的 `ReplyFunc` 与 `StreamReplyFunc` 只传 `sessionKey`，钉钉的 `SenderStaffId` 仅用于建卡，微信群内发送者被直接丢弃。这套可信身份改造无论是否接 OpenViking 都必须先做。叠加上游没有客户端幂等键、commit 为异步且任务记录 TTL 仅 24 小时、删会话不删派生记忆，写入对账与删除闭环是一块独立的工程量。两件难事捆在一起做不划算。

## 2. 核验基线与逐项核验

### 2.1 基线

| 项 | 值 |
| --- | --- |
| OpenViking 源码 | HEAD `c31e261b28fd2112655a62e4c17c8d0caa1e3561`（2026-09-11） |
| 最近发布 | `v0.4.19`（2026-09-08）。tag 到 HEAD 共 41 个 commit，检索、技能、ACL 语义无变化 |
| Go SDK | tag `sdk/go/v0.0.2`（2026-09-10），与 HEAD 的 `sdk/go` 零差异。手写、无重试、无版本协商 |
| Jelly | HEAD `6b4f61d`，ADK-Go `v1.6.0`（README 首行仍写 v1.3.0，需顺带修正） |

上游源码引用形如 `routers/search.py:231`，对应
`https://github.com/volcengine/OpenViking/blob/c31e261b28fd2112655a62e4c17c8d0caa1e3561/openviking/server/routers/search.py#L231`。
上游文档引用形如 `concepts/11-multi-tenant.md`，对应 `https://docs.openviking.ai/zh/concepts/11-multi-tenant`。
Jelly 引用形如 `internal/engine/engine.go:70`，为仓库内相对路径。

上表是**源码核验基线**。**实施锁定版本为 v0.4.17.1**，即已部署实例的版本，已确认先按该版本推进；两者差异与影响见 2.7。不跟随 `latest` 或 `main`。

### 2.2 原方案需改写之处

| # | 原方案说法 | 核验结果 | 依据 |
| --- | --- | --- | --- |
| 1 | Skills 属用户范围，不存在可写的账号公共 Skills 根 | **错**。v0.4.6 起恢复了 `viking://agent/skills` 账号共享根，但它没有 ACL | `routers/skills.py:89-105`；`_access.py:1023-1026`；`storage/acl.py:227-235`；changelog v0.4.6 |
| 2 | 不假定上游原生提供不可变版本仓库，版本与哈希全由 Jelly 补充 | **部分错**。`GET /skills/{name}?include_integrity=true` 返回逐文件 sha256、包级 `revision` 与 `content_sha256`；v0.4.19 已有。但 Go SDK 未封装该参数，需裸 HTTP | `routers/skills.py:406,434-436,489`；`sdk/go/types.go:129-135`；上游 VikingBot 据此 fail-closed，`bot/vikingbot/agent/remote_skills.py:499-537` |
| 3 | 第一阶段不新增 Elasticsearch | 上游**根本不支持 ES**，全仓零命中。向量后端只有 local(RocksDB)、cuvs、http、volcengine、vikingdb；内容后端 local、s3 | `vectordb_config.py:273-279`；`agfs_config.py:386-387` |
| 4 | hybrid 快照另有限制 | 用词错误。实际限制是 OVPack 的 `include_vectors` 拒绝 hybrid 与 sparse 索引。「Snapshot」在上游是另一套基于 git 的历史功能 | `storage/ovpack/vectors.py:65,74,217`；`guides/15-snapshot.md` |
| 5 | 首版由 Jelly 单独调度 commit，关闭另一套自动触发 | 上游自动 commit **默认已关**，两道门：`memory.session_auto_commit.default_enabled=False`，且会话无策略即禁用。改为「防御性显式传 null」而非「关闭」 | `memory_config.py:15-19`；`auto_commit_policy.py:5`；`sdk/go/sessions.go:19-20` |
| 6 | 常规 api_key 模式使用绑定账号的用户密钥 | 对，但漏了两条决定性约束：api_key 模式下 Account/User 头被**静默忽略**；root key 被**禁止**访问租户数据 API。因此「一个机器人凭据服务多个终端用户」只能走 trusted 模式或逐用户发 key | `auth/plugins/api_key.py:93-103,272-281`；`plugins/trusted.py:125-205`；`concepts/11-multi-tenant.md` |
| 7 | 不要直接采用 `search(mode=context)`，因为它不接受 `target_uri` | **对，硬 400**。但漏了它是唯一带 `max_tokens`、`quotas`、`dedup_turns` 的服务端预算装配。真正的结论是：**范围限制与预算装配在服务端互斥**，这决定了本方案的检索形态 | `routers/search.py:218-232`；测试 `tests/server/test_api_search_context.py:186-187`；`sdk/go/retrieval.go:100-148` |
| 8 | 不假定上游提供客户端消息幂等键 | **对**。`turn_id` 与 `source_message_ids` 只是元数据，服务端 id 为 `msg_{uuid4}`，重试即重复。补充：`turn_id` 可用于读回对账 | `routers/sessions.py:128-156`；`session/session.py:1437` |
| 9 | 删除远端会话与删除已抽取记忆是不同动作 | **对**。记忆在 `{user}/memories`，与 `{user}/sessions` 同级。补充：溯源只有每次归档的 `memory_diff.json` 与记忆 frontmatter 的 `source_extraction_id`、`last_update_trace_id`，**没有逐消息溯源** | `service/session_service.py:354-360`；`compressor_v3.py:2153-2178`；`memory_updater.py:1114-1119` |
| 10 | OVPack 不含账号、API Key、运行队列 | 对。补充两项遗漏：**也不含 ACL 授权，也不含 `viking://agent/skills`**，故团队技能的备份只能靠 Git 源与发布记录 | `storage/ovpack/policy.py:21-24`；`operations.py:381-409` |
| 11 | 代码事实 | 三处需修正：`internal/memory/pgsearch.go` **不存在**，PG 分支在 `fts5.go:155-188`；Jelly 的 Skill frontmatter 只有 `name`、`description`、`enabled`，**没有 `allowed_tools`**，原方案关于该字段的承诺是空的；`ENVIRONMENT.md` 是第三个核心记忆文件，且模型**无法写入**（`fileFor` 只映射 memory 与 user，环境另走 `SetEnvironment`） | `internal/skill/skill.go:38-42`；`internal/memory/core.go:239-241,340-349` |

`ENVIRONMENT.md` 的「运维可写、模型不可写」是一条现存安全属性，后续任何迁移都必须保留这个机制，而不只是保留这个文件名。

### 2.3 上游已支持，可直接使用

- **子树范围限制**。`find` 与 `search(mode=list)` 接受 `target_uri`，可为字符串或数组，编译为段感知的路径前缀匹配，深度不限；每个 target 先做访问校验再检索。target 为空时，非 ROOT 默认检索当前用户根与 `viking://resources`。`viking_vector_index_backend.py:2217-2220`；`core/retrieval_targets.py:83`；`_semantic.py:247-248`。
- **元数据过滤**。严格 `k=v` 格式的 `tags`（多个之间是 AND）、`filter` DSL、`since` 与 `until`、`context_type`、`score_threshold`。`routers/search.py:120-140`。
- **导入**。`POST /resources` 接受远程 URL、git、tos，或 `temp_file_id`；目录由客户端打包为 zip 上传，Go SDK 已实现且跳过符号链接。支持 git 仓库、sitemap、RSS、Atom、飞书。异步任务状态机为 `pending`、`running`、`cancelling`、`completed`、`failed`、`cancelled`。`routers/resources.py:222`；`sdk/go/upload.go:143-188`；`service/task_tracker.py:53-58`。
- **分层内容**。L0 `.abstract.md` 上限 256 字符，L1 `.overview.md` 上限 4000 字符，两者都是目录级 sidecar；检索只返回 L0，更深层通过 `/content/abstract`、`/content/overview`、`/content/read` 获取。`concepts/03-context-layers.md`；`routers/content.py:125-177`。
- **删除即删向量**。`DELETE /fs` 先同步删除向量记录再删文件，向量删除失败即抛错；操作幂等，目录需显式 `recursive`。`storage/viking_fs/_ops.py:151-155,250-253`。
- **账号级 ACL**。默认关闭。开启后可按目录授权给 `user:{id}`、`group:{id}`、`user:*`，级别为 `read` < `write` < `manage`，支持继承与 restricted 模式，ADMIN 隐式拥有 manage。只作用于 `viking://resources`，正好覆盖本轮需要。`storage/acl.py:147-156,239-240`。
- **SSRF 防护**。拒绝 localhost 与解析到非全局地址的主机，覆盖回环、内网与云元数据地址，且每个重定向跳转都复检；HTTP、MCP 与 watch 入口强制开启。`utils/network_guard.py:98-153`。

### 2.4 需要 Jelly 实现

- **检索预算与上下文装配**。因为核验 #7 的互斥关系，范围隔离不可让步，所以预算必须自己做。
- **来源登记与导入任务落库**。上游任务记录 TTL 只有完成 24 小时、失败 7 天（`task_tracker.py:186-189`），导入结果必须落到 Jelly 自己的库里，否则事后无法追溯。
- **正文读取自限**。`/content/read` 按行 offset 切片，但服务端是先读整个文件再切，**没有大小上限**（`_ops.py:1676-1680`）。Jelly 必须自己限制行数与字节数。
- **导入源白名单**。上游 SSRF 防护有两个已知缺口：代码托管域白名单会跳过 IP 检查（`parser_config.py:220-230`），主机无法解析时放行（`network_guard.py:128-130`）。因此导入源仍须由 Jelly 侧白名单把关，不能让模型或普通用户指定任意 URL。
- **召回评估与观测**、**降级与冷却**。见第 7 节。
- **就绪探测**。Go SDK 只封装了 `/health`，没有 `/ready`；需要就绪语义时裸 HTTP 调用。

另需注意：检索命中实际只序列化 `uri`、`level`、`score`、`abstract`、`tags`、`context_type`（`openviking_cli/retrieve/types.py:376-383`）。Go SDK 的 `MatchedContext` 里 `Overview`、`Category`、`MatchReason` 三个字段**永远为空**，上游文档 `api/06-retrieval.md` 在这一点上有漂移。Jelly 的结果结构不要包含这三个字段。

### 2.5 部署与数据出网约束

- 镜像 `ghcr.io/volcengine/openviking`，端口 1933，持久目录 `/app/.openviking`。
- `OPENVIKING_WITH_BOT` 在官方镜像里**默认为 1**，必须显式设为 0（`docker/openviking-entrypoint.sh:4,86-88`）。
- Web Studio 常驻挂载在 `/studio`，没有直接关闭的开关（`app.py:793-835`），只能靠网络层收口。
- `/metrics` 默认关闭，一旦开启则**没有鉴权**（`routers/metrics.py:13-27`）。
- **没有高可用**。helm 对 `replicaCount>1` 直接 fail，理由是 RocksDB 不支持多 pod 并发访问；部署策略为 `Recreate`，并有进程级 PID 锁。`deploy/helm/openviking/templates/deployment.yaml:1-2`。多副本共享一个卷不是 HA 方案。
- 上游**没有 PII 脱敏**。它的「privacy」功能是技能字段的密钥存储，不是内容脱敏；而且 `add_skill` 会把技能正文送给 VLM 做密钥抽取，没有开关（`skill_extractor.py:36`）。静态加密默认关闭。
- **文档存在本地，但语义能力依赖模型，所以正文会出网。** 出网点与可避免性见下。

| 时机 | 送出内容 | 目标模型 | 在本方案下能否避免 |
| --- | --- | --- | --- |
| 导入 | 文档正文分块 | embedding | **不能**，这是语义检索的前提 |
| 导入 | 文档正文 | VLM | **不能**，用于生成 L0 `.abstract.md` 与 L1 `.overview.md`，未找到关闭开关（`utils/summarizer.py:32-33` 持有 `VLMProcessor`） |
| 导入 | 图片与 PDF 页面 | VLM | 能，不导入这类文件即可 |
| 每次检索 | 查询文本 | embedding | **不能**，dense 检索必须嵌入查询 |
| 不触发 | 会话历史 | VLM | `retrieval.enable_intent` 默认为 true（`retrieval_config.py:39`），但该路径只在传 `session_id` 时进入（`_semantic.py:426-446`）。本方案用 `Find` 且不创建 OpenViking 会话，故不触发 |
| 不触发 | 候选文档正文 | rerank | rerank 的 `provider` 默认为空即不启用（`rerank_config.py:11-13`），且 `find` 固定 QUICK 模式从不重排 |
| 不触发 | 技能正文 | VLM | 本轮不迁移 Skills |

后三行的「不触发」是本方案检索形态的直接结果，其中会话历史那一条尤其值得留意：它默认开启，只是被我们「用 `Find`、不建远端会话」的选择绕开了。这一点要写进契约测试，避免将来有人顺手改用 `search(session_id=...)` 而把事故对话里的主机名与日志片段送出去。

**全部指向自建模型是可行的，代价是只能用 dense 单路检索。** embedding 的 provider 列表含 `ollama`，也可用 `openai` 配合 `api_base` 指向自建的 OpenAI 兼容端点（`embedding_config.py:168-182`）；VLM 有 `api_base` 与 `providers` 字典，`openai` 与 `litellm` 同样可指向自建端点（`vlm_config.py:117-151`）。两处代价与限制：`local`（GGUF）embedding 依赖 `llama-cpp-python` 这个可选 extra（`pyproject.toml:186-188`），不能假定官方镜像里已安装，需自行确认或自建镜像；sparse 与 hybrid embedding 只有 volcengine 与 vikingdb 支持，因此全自建等于放弃混合检索，只跑 dense。

**边际暴露的判断。** Jelly 现在已经把用户问题、`ENVIRONMENT.md` 内容和工具返回的真实日志送给在线模型，出网通道早就存在且更宽。OpenViking 新增的是两件不同性质的事：一是整个文档语料在导入时被**系统性地、一次性地**送出，包括从没人问过的文档，这与「按事件被动带出」的风险画像不同；二是多一个供应商关系与一份数据处理约定。这两件事应分别决策，不能用「反正 Agent 已经在用在线模型」一句话带过。

**四种模型部署组合的暴露范围。** 关键前提是 VLM 的输入就是文档正文：`_generate_file_summary` 读取整个文件、截到 `semantic.max_file_content_chars`（默认 30000 字符，`parser_config.py:693`）后直接渲染进 prompt（`semantic_processor.py:972-1038`）。对多数运维手册这就是全文。因此「VLM 用线上、embedding 用本地」并不能让文档留在内网。

| 组合 | 文档正文 | 查询文本 | 说明 |
| --- | --- | --- | --- |
| 都本地 | 不出网 | 不出网 | 代价是自建两个服务，且只能跑 dense |
| embedding 本地 + VLM 线上 | **出网**（导入时） | 不出网 | 查询不出网是实质收益；文档仍然出网 |
| 都线上 | 出网 | **出网**（每次检索） | 最省事，质量最好 |
| embedding 本地 + 不配 VLM | 不出网 | 不出网 | 已核验可行，代价是摘要降级 |

**已选第二行**，决定与后果见 7.4。它确实赢下了一样东西，**查询文本不出网**。查询是持续流动的那一路，事故中提出的问题本身就携带信息，例如服务名与故障现象。相比两个都用线上，这是实质改进；它只是没有达成「文档留在内网」。若按成本与延迟而非隐私来选，这一行也是合理的：embedding 在查询路径上，本地化省掉每次检索的网络往返与调用费用；VLM 只在导入时跑，线上大模型的摘要质量更好且不影响查询延迟。

另一个事实：代码文件若 AST skeleton 抽取成功则不调用 VLM（`semantic_processor.py:993-1000`），但 Markdown 手册属于 documentation 类型，必定调用。本方案的语料躲不开这一步。

**因此建议由阶段 A 决定是否需要 VLM。** 阶段 A 本就只导入少量脱敏文档，正好可以直接测「没有摘要时 30 题命中率能否过 85%」。若能过，则不需要 VLM，隐私与硬件两个问题同时消失；若不能过，此时拿到的是「摘要值多少命中率」的实测差距，再据此决定是否引入线上或本地 VLM，而不是现在凭判断选。

#### VLM 到底做什么，以及能否不配

上游的「VLM」是当通用 LLM 用的，不只处理图像（`openviking/models/vlm/llm.py`）。这解释了一个常见困惑：即使完全不导入图片，摘要生成仍然需要它。全部用途及本方案是否触发如下。

| 用途 | 实现位置 | 本方案是否触发 |
| --- | --- | --- |
| 图片理解、表格理解、PDF 页面理解、筛除无意义图片 | `parse/vlm.py`，经 `utils/resource_processor.py:93`、`media_processor.py:92` | 仅当导入图片或 PDF；纯 Markdown 不触发 |
| 生成 L0 `.abstract.md` 与 L1 `.overview.md` | `storage/queuefs/semantic_processor.py` | **会触发，且是唯一真正触发的一项** |
| 查询意图分析、上下文改写 | `retrieve/intent_analyzer.py`、`retrieve/context_assembler/rewrite.py` | 不触发，本方案用 `Find` |
| 记忆抽取、会话 checkpoint 摘要 | `session/memory/extract_loop.py`、`session/session.py` | 不触发，不创建远端会话 |
| 技能字段密钥抽取 | `privacy/skill_extractor.py`、`utils/skill_processor.py` | 不触发，本轮不迁 Skills |
| 评估与训练 | `eval/ragas/`、`session/train/` | 不涉及 |

**「必须配置 VLM」是官方姿态，不是运行时约束。** 两者需要分清，因为文档与向导会给人相反的印象：

- 官方姿态：安装向导把 VLM 的 `api_base` 与 `model` 当必填项（`openviking_cli/setup_wizard.py:1256`、`1266`），所有 quickstart 与 helm 示例都带 `vlm` 段（`getting-started/02-quickstart.md:130`、`guides/03-deployment.md:340`），文档把它定位为「用于图像和内容理解」。
- 运行时实际：`VLMConfig.is_available()` 只判断是否解析出 API key（`vlm_config.py:731-739`）；摘要生成的两处调用都先判断再调用，不可用时返回空串或占位符并只打 warning（`semantic_processor.py:1018-1021`、`1280-1282`）。未发现任何强制 VLM 的启动或配置校验。
- 唯一的硬要求在本方案范围之外：`session.py:4432` 会抛 `ValueError("A configured VLM is required to generate checkpoint summaries")`，那属于会话 checkpoint 摘要，本方案不创建远端会话故不会触及。

补充一条：代码文件若 AST skeleton 抽取成功则跳过 VLM（`semantic_processor.py:993-1000`），但 Markdown 属于 documentation 类型，必定调用。

**上述结论来自代码分支阅读，未实际运行过无 VLM 的服务端**，因此必须由阶段 0 第 10 项实测确认，不能直接当作既成事实。

**脱敏必须发生在导入前。** 上游没有内容脱敏，且内容一旦入库就会被原样嵌入与摘要。因此脱敏是 Jelly 导入管线的职责，且是唯一的边界；入库后再补救需要删除并重新导入。

**VLM 缺失时不会崩，但会降级；这让无 GPU 部署成为可能。** 三条已核验的事实决定了这个结论：

| 事实 | 依据 |
| --- | --- |
| `embedding.text_source` 默认 `content_only`，文件级向量取**原文**而非 LLM 摘要 | `embedding_config.py:650-653`；`guides/01-configuration.md:218` |
| VLM 不可用时，文件摘要返回空串，目录 overview 返回 `[Directory overview is not ready]` 占位符，仅告警不抛错 | `semantic_processor.py:1018-1021`、`1280-1282` |
| 目录级 L0/L1 的向量来自 `.abstract.md` 与 `.overview.md` 正文 | `embedding_utils.py:357-378`（`vectorize_directory_meta`） |

因此不配 VLM 时：文件级语义检索**照常工作**，因为它不依赖摘要；但命中的 `abstract` 字段为空，目录级摘要变成占位符且其向量无意义。

这对本方案有一个直接设计后果：6.1 节「注入 L0 abstract」在无 VLM 时无内容可注入。届时退化为注入 URI、标题与 tags，正文一律经 `knowledge_read` 按需读取。这个退化路径应与主路径一起实现，而不是等真遇到才补。

**自建的硬件分工。** 两个模型的要求完全不同，应分开决策：

| 角色 | 是否在查询路径上 | 何时运行 | 规模 |
| --- | --- | --- | --- |
| embedding | 是，每次检索都要嵌入查询 | 导入（批量）与检索（交互） | 小。中文场景的常见选择在几亿参数量级 |
| VLM | 否 | 只在导入与 reindex 时 | 大。生成式调用，输入是文档正文 |

embedding 放 CPU 是可行的：查询是一段短文本，单次嵌入的开销远小于本方案 1.5 秒的召回预算；批量导入是离线任务，慢一些可接受。实际延迟必须在阶段 A 实测，不能照搬这里的判断。VLM 放 CPU 则是生成式长输入长输出，单次调用可能到分钟级；它只在导入时跑，小语料尚可忍受，但更务实的做法是要么用一块入门级 GPU 承载一个小模型，要么先不配 VLM、接受上面的降级。

接入方式优先选 `openai` provider 配 `api_base` 指向自建的 OpenAI 兼容端点，而不是 `ollama` 专用 provider：前者是上游最通用、被最多部署验证过的路径，也便于将来换推理后端。`ollama` provider 确实存在（`embedding_config.py:168-182`），但本次未核验其实现细节。

**两个与硬件无关、但会影响效果的坑。**

第一，关于长文档截断：**源码层面确实是截断而非分块**，`truncate_embedding_input` 用二分把文本裁到 `max_input_tokens`（默认 4096）并加后缀（`utils/embedding_input.py:27-45`）。但阶段 0 实测表明**导入管线会先把长文档拆成多个文件**，因此该截断在正常导入路径上基本不生效，详见 2.7。原先「长手册只有开头进向量」的判断已被实测推翻。

第二，embedding 模型的选择近乎一次性。`dimension` 必须与模型匹配，而 OVPack 复用 dense 向量要求 provider、model、input 与维度逐项一致，不一致就只能重新向量化（`storage/ovpack/vectors.py:193-198`）。换模型等于全量重建索引，所以首次选型要慎重，并把 provider、model 与 dimension 一起记进部署记录。

**选型约束：优先选对称模型。** `query_param` 与 `document_param` 映射的是 API 参数，OpenAI 兼容路径下对应 `input_type`，Jina 对应 `task`（`embedding_config.py:48-67`），**不是拼在文本前面的指令前缀**。因此凡是要求「查询侧文本前加一段中文指令」的模型，在 OpenViking 配置层无法表达，只能靠推理服务自己补，或接受效果损失。选一个查询与文档同构、不需要前缀的模型可以整体绕开这个问题。

综合上述三条约束（截断而非分块、维度近乎一次性、无法注入文本前缀），候选如下。**最终由阶段 A 的 30 题命中率决定，不以此处排序为准**，且必须在语料还小的时候测完，因为换模型等于全量重建。

| 候选 | 关键属性 | 取舍 |
| --- | --- | --- |
| bge-m3（首选） | 约 5.6 亿参数；上下文 8192；对称、无需前缀；1024 维；多语言含中文 | 长上下文直接缓解截断，可把 `max_input_tokens` 提到 8192；对称特性匹配上游能表达的范围；各推理栈普遍支持。它的 sparse 与 colbert 输出用不上，因为上游 sparse 只支持火山 |
| Qwen3-Embedding-0.6B（备选，值得同场测） | 参数量相近；上下文更长；维度可配 | 查询侧建议带 instruct 前缀才发挥最佳，而上游注入不了文本前缀，需推理层自行补或接受损失 |
| 512 上下文的中文模型（不推荐） | 例如 bge-large-zh-v1.5、bge-base-zh-v1.5 | 512 上限叠加「截断而非分块」会让长手册大段不可检索。除非文档已拆得很细，否则不选 |

VLM 只影响目录级摘要与命中的 `abstract` 字段，文件级检索不依赖它，因此属于低风险选择，任务本身也简单：把一篇手册压成 256 字以内的摘要。入门级 GPU 上用 40 亿参数量级的中文指令模型做 4-bit 量化即可，不必在这里加预算；纯 CPU 则建议先不配，走前面的降级路径。

配置层面需要一并固定并记入部署记录的字段：`provider`、`model`、`dimension`、`input`、`max_input_tokens`。其中 `input` 默认值是 `multimodal`（`embedding_config.py:47`），纯文本 embedding 模型应显式改为 `text`，否则可能按多模态路径发请求。接入方式用 `provider: openai` 配 `api_base` 指向自建端点；推理服务可选专为 embedding 优化且有 CPU 镜像的方案，不必用通用对话推理栈。
- 没有回传遥测。`usage_reporter` 默认关闭，且只有 `file_log` 与 `custom` 两种 sink（`server/config.py:261-266`）。
- Agent Evolution 默认关闭（`server/config.py:129-132`）；`memory.session_skill_extraction_enabled` 默认关闭，一旦开启会在 commit 时自动改写用户根下的技能（`memory_config.py:84-90`）。两者都必须保持关闭并写进部署检查单。

### 2.6 待验证

这三项本轮未能从源码确定，在阶段 0 的契约测试里固定实际行为，不靠假定。

1. `tests/server/test_auth.py:502-593` 期望 api_key 模式下改 Account/User 头返回 403，而 `auth/plugins/api_key.py:97-103` 的实现是静默忽略。两种行为下都无法冒充他人，但需要测出实际行为再依赖它。
2. 消息 role 是否在运行时拒绝非 `user` 与 `assistant`。本轮不写会话，留到记忆阶段。
3. 缺少 VLM 配置时是否拒绝启动；VLM 后端 tracer 记录完整 prompt 的默认落点。

### 2.7 已部署实例实测（2026-09-11）

对已搭建实例完成了阶段 0 中全部只读项。**未写入任何数据。**

| 项 | 实测结果 |
| --- | --- |
| 端点 | `http://172.16.5.128:1933` —— **明文 HTTP，且没有 `/openviking` 路径前缀** |
| 版本 | `v0.4.17.1`，**不是本文核验基线 v0.4.19** |
| 认证模式 | `auth_mode: api_key`，即已配置 `root_api_key`，未落在开发模式。符合 4 节的前提 |
| 账号 | 仅 `default` 一个，`user_count: 1`，创建于 2026-09-10 |
| 凭据 | `~/.openviking/ovcli.conf` 中的是 **root key**；另已存在 user `jelly-agent`（role `user`），其 key 可用 root key 经 `GET /api/v1/admin/accounts/default/users` 只读取回 |
| 模型 | VLM `qwen3.8-flash`（provider `openai`）；embedding `qwen3.7-text-embedding-flash`（provider `openai`，已有 26 次调用） |

**四个需要处理的差异。**

1. **`ovcli.conf` 的 URL 两处都不对**：写的是 `https://172.16.5.128:1933/openviking`，而实例是明文 HTTP 且无路径前缀。`/health` 在 `http://172.16.5.128:1933/health` 返回 200，带 `/openviking` 前缀则 404。Jelly 的 `endpoint` 应配 `http://172.16.5.128:1933`。跨主机访问仍需按 2.5 加 TLS 与网络限制。
2. **root key 不能用于 Jelly。** 已在该实例上实测确认 2.2 核验 #6：以该 key 请求 `/api/v1/fs/ls` 返回 403，消息为 `ROOT API keys cannot access tenant-scoped data APIs in api_key mode`，与源码 `auth/plugins/api_key.py:278` 一致。**Jelly 必须使用 user key。** 该实例已存在 user `jelly-agent`（role `user`），其 key 可用 root key 经 `GET /api/v1/admin/accounts/default/users` 只读取回，无需新建用户。Jelly 的 `api_key` 配这一把，root key 不进 Jelly 配置。
3. **部署与 7.4 决定不一致，需改部署。** 当前 embedding 也是线上服务（`qwen3.7-text-embedding-flash`），而 7.4 定的是 embedding 自建。**已确认维持 7.4**，因此需把服务端 `embedding` 段改指自建端点。必须在导入正式语料前完成：换模型会触发全量重建并连带重跑全部 VLM 摘要。当前 `viking://resources` 为空，现在切换成本最低。
4. **v0.4.17.1 没有 ACL。** 该版本不存在 `openviking/storage/acl.py` 与 `openviking/server/routers/acl.py`，`account_settings.py` 只有 `agent_evolution` 而无 acl 开关；ACL 系列特性是 v0.4.17.1 之后加入的。后果是 `viking://resources` 在账号内**没有任何目录级授权**，唯一隔离边界是 `account`。单一运维团队不受影响；若需多团队分权，只能升级版本或改用独立 account，而后者无法共享 resources。4 节关于「启用账号级 ACL 并逐团队发 key」的写法仅适用于 v0.4.19 及以上。**已确认先按 v0.4.17.1 推进**：阶段 A、B 只面向单一运维团队，不依赖 ACL；多团队分权列为需升级的后续项。实施锁定版本因此为 v0.4.17.1，而非 2.1 的核验基线 v0.4.19。

**在 v0.4.17.1 上已确认成立的承重事实**（按标签比对源码）：`embedding.text_source` 默认 `content_only`（`embedding_config.py:649`）；`mode='context'` 拒绝 `target_uri`（`routers/search.py:234`）；root key 禁访租户数据 API（`api_key.py:278`）；VLM 不可用时降级为空摘要与占位 overview（`semantic_processor.py:1006`、`1268`）；`include_integrity` 已存在。`FindRequest` 在该版本已含本方案需要的全部字段：`target_uri`、`tags`、`filter`、`score_threshold`、`context_type`、`level`、`since`/`until`、`include_provenance`、`read_content`。

**SDK 兼容性：已实测通过。** `sdk/go/v0.0.2` 相对 v0.4.17.1 在检索路径上的唯一变更是 `Grep` 与 `Glob` 新增 `tags`/`include_tags`；`Find` 与 `Search` 未变，且字段均为条件写入。已按 `Find` 的完整 payload 实跑确认不触发 `extra="forbid"`（见下表）。仍建议在该版本上避免使用带 tags 的 `Grep`/`Glob`。

**已完成的阶段 0 只读核查**（凭据用 user `jelly-agent`，全部在该实例上实跑）：

| 核查 | 结果 |
| --- | --- |
| 身份分层（D1） | root key 请求 `/api/v1/fs/ls` 返回 403 并给出 `ROOT API keys cannot access tenant-scoped data APIs`；同一请求换 user key 返回 200。**D1 成立** |
| `mode='context'` 拒绝 `target_uri` | 返回 400 `INVALID_ARGUMENT`，原文 `target_uri is not supported in mode='context'`。**D2 的承重约束成立** |
| context 模式本身可用 | 不带 `target_uri` 返回 200，`entries` 形如 `{uri, category, score, detail, text, origin}`，且同时命中 `viking://resources` 与 `viking://user/...`，无法限定范围。这正是本方案不用它做召回的原因 |
| `find` + `target_uri` | 返回 200，桶结构 `memories / resources / skills / total` |
| **命中字段实测** | 仅 `context_type`、`uri`、`level`、`score`、`abstract`、`tags` 六项。**无 `overview`、`category`、`match_reason`**，证实 2.4 关于 Go SDK 三字段恒空的判断 |
| 跨用户 `target_uri` | 返回 403 `PERMISSION_DENIED: Access denied for viking://user/someone-else/memories`。范围隔离在服务端强制，不是结果过滤 |
| 跨用户 `fs/ls` | 同样 403。与检索结论一致 |
| `/ready` | 存在且免鉴权，返回分项检查 `agfs`、`vectordb`、`api_key_manager`、`embedding`、`ollama`。Go SDK 未封装，用裸 HTTP 调用 |
| SDK 等价 payload 兼容性 | 发送 `Find` 会带的全部条件字段（`target_uri` 数组、`limit`、`score_threshold`、`context_type`、`include_provenance`、`read_content`、`tags`、`level`）返回 200，**未触发 `extra="forbid"`**。`sdk/go/v0.0.2` 的 `Find` 可用于 v0.4.17.1 |

一条新发现的设计细节：`find` 会把目录 sidecar 本身作为命中返回，例如 `viking://resources/.abstract.md`（`level: 0`）。这类命中对运维问答没有直接价值，`internal/knowledge` 需要决定是过滤掉还是作为目录级导航单独呈现，不能与文档命中混在一起注入。

**已完成的阶段 0 写入核查**（经授权导入两篇一次性测试文档，测毕已全部删除并复查实例恢复原状）：

| 核查 | 结果 |
| --- | --- |
| 导入任务状态机 | `add_resource` 返回 `{status:"success", task_id, root_uri}`。注意 **v0.4.17.1 返回的是 `success` 而非文档所述 `accepted`**，代码不可硬判 `accepted`。任务状态经 `running`/`processing_queue` 收敛到 `completed`/`completed`。短文档约 20 秒，长文档约 50 秒 |
| **长文档自动分块** | 83730 字节的单文档被拆成 **10 个约 8.7 KB 的文件**，头尾内容分别落在 `_1.md` 与 `_10.md`，各自独立向量化。**这推翻了 2.5 原先「截断导致尾部不可检索」的判断**：截断在源码层面为真，但导入先分块，正常文档不受影响 |
| 实际 URI 与 `to` 不一致 | 指定 `to` 为 `.../long` 时，内容落在 `long/<文档标题>/<文档标题>/*.md`，嵌套两层。登记必须以响应 `root_uri` 为准并事后发现叶子 URI |
| 检索质量 | 真实中文查询下目标文档稳居首位：「支付网关返回 502 应该怎么排查」命中短文档 0.713；「集群扩容时要检查哪些资源水位」命中长文档分块 0.746 |
| `abstract` 字段 | 配置 VLM 时内容详实，L2 文件命中也带完整摘要，可直接用于 6.1 的注入 |
| 目录 sidecar 混入命中 | 已复现：`.abstract.md` 与 `.overview.md` 与文档命中同列返回，`internal/knowledge` 必须区分处理 |
| tags 端到端 | `set_tags` 成功并在命中中回显；按 `env=prod` 过滤精确收敛到 1 条，按 `env=staging` 过滤返回 0。**§5 的结构化过滤方案成立** |
| `DELETE /fs` 同步删向量 | 删除前检索 16 条命中，递归删除后降为 0，`resources` 目录清空。**核验 #9 前半段成立** |

**一项计划外的重要发现：导入会自动写入用户记忆。** 传了 `reason` 参数后，`add_resource` 会把资源摘要经一次完整 session commit 写进调用者的 `viking://user/{user}/memories/`，实测产生了 `events/2026/09/11/*.md` 两条事件记忆，以及 `identity.md` 与 `soul.md` 两个画像文件；事件记忆正文包含文档内容的 VLM 摘要。删除资源时响应中的 `deleted_memory_uris` 为空，**这些记忆不会被一同删除**，同时正面印证了核验 #9「删资源不删派生记忆」。

触发条件是单一的：`resource_memory_link_service.py:157-159` 中 `if not reason: return {"status":"skipped","reason":"empty_reason"}`。**因此导入时不传 `reason` 即可完全避免**，无需改服务端配置。这一条已写入 5 节导入流程。若将来要用 `reason`，须同时承担三件事：额外的 VLM 调用成本、用户记忆空间中出现文档内容副本、以及删除资源后需单独清理这些记忆。

**仍未完成的阶段 0 项目**：VLM 不可达时的故障面（需把服务端 `vlm` 指向不可达端点并重启，属服务端变更，未执行）；embedding 选型对比与 30 题命中率（须在 embedding 切换为自建后进行，否则测的不是最终配置）。

## 3. 分工边界

| 数据或能力 | 归属 | 理由 |
| --- | --- | --- |
| Agent 编排、子 Agent、工具执行、审批 | Jelly | 不引入第二套执行运行时 |
| 当前对话、任务状态、审计记录 | 现有 SQLite / PostgreSQL | 保持现有恢复、查询与审计语义 |
| 运维手册、架构文档、已确认故障案例 | OpenViking `viking://resources/ops` | 本轮接入的全部内容 |
| 知识来源登记、导入任务结果、召回评估 | Jelly 数据库 | 上游任务记录会过期，且不支持自定义 metadata |
| 系统指令、执行权限、安全策略 | Jelly 配置 | 不是可被检索结果覆盖的内容 |
| `MEMORY.md` / `USER.md` / `ENVIRONMENT.md` | 保持现状，本轮不动 | 迁移前置条件未满足，见 9.5 |
| Skills 正文、辅助文件与执行 | 保持现状，本轮不动 | 上游共享根无授权控制，见 9.5 |
| L2 全文会话检索 | 保留，作为独立可配置能力 | 与知识检索语义不同，两套结果不混注 |
| 日志、指标、Trace、实时服务状态 | 原有观测平台与工具 | 知识用于提出排查路径，实时工具用于验证 |
| 工具原始结果与证据句柄 | 现有 `internal/record` 与 gateway | 不把大量日志重复写进知识库 |

部署关系：

```text
CLI / Web / 钉钉 / 微信 / 定时任务
                 │
          Jelly Agent 运行时
          ├─ 工具网关 → 运维系统 / 审计
          ├─ 本地数据库 → 会话 / 任务 / 证据 / 知识来源登记
          └─ 知识适配层 → OpenViking HTTP（只读检索 + 受控导入）
                              ├─ viking://resources/ops
                              ├─ 向量索引（RocksDB）
                              └─ Embedding / 内容处理模型
```

## 4. 身份与权限

**决定：api_key 模式，单一运维 user key，Jelly 不持 root key。**

服务端配置 `server.root_api_key` 以离开开发模式。开发模式下所有请求都被视为 ROOT，默认身份 `default/default`，且只允许绑定 localhost（`concepts/11-multi-tenant.md`），不能作为正式部署。管理员用 Admin API 创建 account 与一个普通 user，Jelly 只持有该 user key。`viking://resources` 在 account 内默认共享，该身份即可读取 `viking://resources/ops`。

这比 trusted 模式权限更小：Jelly 不保管 root key，也不需要由自己断言身份。trusted 模式是唯一支持「一个凭据代理多个用户」的路径，但那是长期记忆阶段才需要的能力，本轮不引入。

硬要求四条：

1. OpenViking 的 1933 端口只对 Jelly 所在内网开放，跨主机加 TLS 与访问限制。`/studio` 与 `/metrics` 一并由网络层收口。
2. root key 只用于 Admin API，与业务 key 分开保管，不作为日常数据访问凭据。
3. 模型不能选择身份、账号、凭据或扩大检索范围。授权范围必须写在请求里，上游的身份与 ACL 是第二道隔离，不能只靠结果返回后过滤。
4. 多团队分权时，启用账号级 ACL 并逐团队发 key，绝不靠目录命名、标签或提示词充当访问控制。**该做法要求 v0.4.19 及以上**；当前部署的 v0.4.17.1 没有 ACL，唯一隔离边界是 `account`，详见 2.7。

缓存按账号、身份、授权范围、修订与 invocation 隔离，不跨身份共享结果。

## 5. 知识组织与导入

首批只导入运维手册、系统架构与已确认的复盘文档。逻辑目录：

```text
viking://resources/ops/
├─ runbooks/        排查、回滚、恢复手册
├─ architecture/    服务关系与架构说明
├─ environments/    非敏感环境说明，不是实时 CMDB
└─ cases/           经过确认的故障案例
```

每篇文档打严格 `k=v` 格式的 tags，例如 `env=prod`、`svc=gateway`、`kind=runbook`。tags 在检索时是 AND 关系，是本方案唯一可用的服务端结构化过滤手段，因为上游 `add_resource` **没有自定义 metadata 字段**，可附加的只有 tags、`reason`、`instruction`、`source_name` 与解析器参数。

每篇文档在 Jelly 侧登记：来源标识、来源 URL 或仓库路径、原始修订号、内容哈希、负责人、tags、OpenViking URI、导入任务 id 与状态、导入时间与撤销时间，以及**是否已过脱敏与由谁确认**。最后一项因 7.4 的决定成为必填：导入即等于把正文送给线上 VLM 提供商。原始文档继续以 Git、文档系统或对象存储为事实源。

导入流程：管理员从白名单来源选择 → 大小与类型校验、脱敏 → `temp_upload` 上传 → `add_resource` 导入（**不传 `reason`**）→ 轮询任务至 `completed` → 发现实际叶子文件 URI → `content/set_tags` 打标签 → 校验可检索 → 登记并发布该修订。

三个由阶段 0 实测确定的约束（详见 2.7）：

- **不能在导入时打标签。** v0.4.17.1 的 `AddResourceRequest` 没有 `tags` 字段，必须导入完成后调 `POST /api/v1/content/set_tags`。因此存在「已索引但未打标」的时间窗，来源登记须记录标签是否已补齐。
- **不能假定最终 URI 等于 `to`。** 实测指定 `to` 为某目录时，内容会落在其下以文档标题命名的嵌套子目录中。必须以响应里的 `root_uri` 为准，并在任务完成后列出实际叶子文件 URI 再登记。
- **不要传 `reason`。** 该参数会触发上游把资源摘要写入调用者的用户记忆空间，见 2.7。

替换文档时，在新修订就绪后再切换有效版本。被撤销的修订立即从 Jelly 的可用范围排除并失效缓存。注意上游 `add_resource` 的 `to` 参数是**破坏性同步**：目标会被同步成与新来源一致，目标里新来源没有的条目会被删除；不带 `to` 时则永不覆盖，而是自动加 `_1`、`_2` 后缀。两种语义都要在导入工具里显式选择，不能靠默认。

本轮不开放全盘目录扫描，不让在线 Agent 自动向知识库写入，不让远端服务访问模型提供的任意 URL、本机路径或内网地址。

## 6. 每轮检索与工具

### 6.1 自动召回

**形态：`Find(target_uri + tags)` 限范围，Jelly 自己做预算与装配。** 直接理由是核验 #7 的互斥关系：`mode=context` 是唯一带 `max_tokens` 的服务端装配，但它不接受 `target_uri` 且返回硬 400；而范围隔离不可让步，所以选 `Find` 并自己装配。

**注入点：root agent 的 before-model 回调**，即 `internal/engine/engine.go:1137` 的 `modelCallbacks`，把参考材料作为**瞬态内容**加到请求里，随后返回 `nil, nil`。三条理由：

- 不落 session 事件，因此不进入会话历史，也不会被将来的记忆捕获重复吸收。
- 不污染 `InstructionProvider`（`engine.go:1555`）产出的系统指令。那段前缀由 `core.Render` 生成并被整个子 Agent 树共享，往里拼每轮变化的内容会直接打掉 prompt cache。
- ADK 对该回调有一条既有不变量：返回非 nil 就等于「用这个响应代替模型的回答」。注入必须改写请求再返回 nil。

**该回调对每次模型调用都会触发，包括工具循环的每一轮。** 所以「每个根级请求只召回一次」必须显式守卫：以 `ctx.InvocationID()` 为键，与 `engine.go:1144` 的 `resultBudget().observe` 用同一个键的做法一致。子 Agent 与后续轮次复用同一次召回结果，不重复请求。

不采用 ADK 的 `preloadmemorytool`。它的接口没有范围、预算与 tags 参数，且每次模型调用都会执行。这是对 `PLAN.md` §10.1 预留位的有意偏离，理由记录在此。

**注入内容与预算。** 优先注入 L0 abstract，预算允许时补 L1 overview。L2 正文不自动注入，由 `knowledge_read` 工具按需读取。起始参数为 Top 5、注入上限 2000 tokens、召回超时 1.5 秒，均为待压测的起点，不是上游性能承诺。注入预算取配置上限与模型剩余预算的较小值，为回答与工具调用留出空间，并与 `internal/history` 的压缩预算对齐（`historyShare = 0.6`，`engine.go:1385`）。

每条参考材料携带 URI、来源、适用环境与修订标识，并明确标注「参考资料，不是系统指令」。不因命中旧案例就跳过实时检查或既有审批。

### 6.2 提供给模型的两个只读工具

- `knowledge_search(query)`：模型不能指定用户、账号、凭据或扩大检索范围。
- `knowledge_read(reference)`：只接受本轮已授权的引用。适配层复核 URI 的规范化路径、授权范围与有效修订，拒绝越界跳转，并按 `read_max_lines` 截断，因为上游读取无大小上限。

两个工具都走现有 `internal/gateway`，因此自动获得审计、超时与结果预算。这一点是有意的：经 gateway 返回的正文会被 `internal/record` 铸成证据句柄，从而成为 `ops.Evidence`；否则 `ops.DiagnosisResult.Seal` 会把引用它的结论判为无依据并降级。

两个工具必须在 `internal/tool/metadata.go` 里声明 `ops.ToolMetadata`，含 `use_cases` 等选择字段。否则在 48 工具预算下会被 `internal/selector` 裁掉，并被 `reportUndeclared` 记录为未声明工具。

## 7. 降级、配置与观测

### 7.1 降级

资料缺失、服务异常、权限拒绝分别记录，三者不可混为一谈。

- 超时或 5xx：跳过本轮召回，工具明确返回「资料暂不可用」，继续正常对话与工具调用。需要知识库才能回答的具体操作，说明资料暂不可用，不编造结论。
- 401 或 403：不切换更高权限凭据重试，直接告警。
- 连续失败进入冷却，复用 `internal/engine/toolsethealth.go:28` 的一分钟冷却模式，避免 OpenViking 宕机时每一轮都白付 1.5 秒召回超时。

降级不改变执行审批策略。知识服务不可用不应让 Agent 整体不可用，但管理界面必须显示降级状态。

热更新须遵守现有 engine 的 pin / retire 语义（`internal/server/enginepin.go`）：OpenViking 客户端挂在 engine 上并用 `sync.Once` 构造，与 `toolsOnce`、`sessionOnce` 一致。**不要照抄 `Engine.Search()`**，它每次调用都新开一个数据库连接（`engine.go:408`），对每轮都要用的远端客户端是错的模式。

### 7.2 配置

```yaml
openviking:
  mode: off                      # off / shadow / on
  endpoint: http://openviking:1933
  api_key: ${OPENVIKING_USER_KEY}
  knowledge:
    roots:
      - viking://resources/ops
    tags: []                     # 追加的全局 k=v 过滤，可空
    top_k: 5
    max_tokens: 2000
    timeout: 1500ms
    read_max_lines: 400
```

`off` 完全不访问 OpenViking。`shadow` 只做独立召回并记录评估数据，不向模型注入、不暴露新工具。`on` 才参与回答。

`mode` 落到 `string` 字段时，`off` 与 `on` 会被 yaml.v3 解析成字符串而不是布尔值，已用本仓库的依赖实测确认，因此示例里不必加引号。

没有 `backend` 字段，也不引入后端抽象：只有一个实现，多一层只会掩盖上游语义。密钥沿用现有的 `${ENV}` 窄展开（`internal/config/config.go:16,429`），不能改用 `os.ExpandEnv`，那会吃掉 bcrypt 哈希里的 `$2a$`。`Save`（`config.go:439-512`）必须为新节补非零判断，否则控制台一次保存就会静默丢配置，Storage 的 DSN 已经踩过这个坑。L2 全文检索的开关独立保留，两套结果不混注。

### 7.3 观测

复用现有观测体系，记录召回耗时、错误分类、命中率、注入 tokens、引用的资料 URI、导入任务状态。默认不记录知识正文与对话正文；来源 URI 按敏感性脱敏。

注意 `docs/observability.md` 已经写明的风险在这里会放大：`tracing.capture_content` 会把 prompt 经 OTel logs 通道送出，而注入的知识正文此后就在 prompt 里。是否开启按环境决定，脱敏放在 collector。

召回评估数据落 Jelly 自己的表而不是只进 telemetry，因为 `internal/telemetry` 是采样、短时、面向舰队的，而阶段 A 的命中率评估与事后追溯需要持久、不采样的记录，这正是 `internal/metrics` 的定位（`internal/telemetry/tracing.go:19-24`）。新增计数时按 `internal/metrics/query.go:12-19` 的告诫标注口径，避免与 `/api/stats` 看起来互相矛盾。

### 7.4 模型部署决定

**已定：embedding 自建，VLM 用线上服务。** 对应 2.5 四组合表的第二行。

选择理由有三条，都不是隐私理由：

- embedding 在查询路径上，每次检索都要嵌入查询。本地化省掉每次检索的网络往返与按次计费，且把延迟控制在自己手里，这对 1.5 秒召回预算是直接收益。
- VLM 只在导入与 reindex 时运行，不影响查询延迟。线上大模型的摘要质量明显优于能在入门级硬件上跑的小模型，而摘要决定目录级向量与注入用的 `abstract` 字段。
- 保留了导入图片与 PDF 的能力。运维手册常带截图与 PDF，这类解析必须有 VLM（`parse/vlm.py`），自建则需要可跑视觉模型的 GPU。这条是该组合相对「不配 VLM」的实质优势。

**已知代价：文档正文会送到 VLM 提供商。** 这不是副作用而是该方案的固有属性，因为 VLM 的输入就是文档正文，单文件上限 30000 字符。因此以下三条从建议升级为硬要求：

1. **脱敏是唯一边界，且必须在导入前完成。** 上游没有内容脱敏，入库即被原样摘要。脱敏规则、负责人与抽检方式需在阶段 A 前确定并写入导入流程，不能靠导入者自觉。
2. **可送出范围需逐目录明确。** 允许进入 `viking://resources/ops` 的文档等价于允许送给 VLM 提供商。凡不允许出网的内容不得导入，也不能靠「不检索它」来规避。
3. **来源登记须记录该篇是否已过脱敏与由谁确认**，作为事后追溯依据。这一项并入 5 节的来源登记字段。

**运维后果：两个模型的重要性与故障面不同。**

| | embedding（自建） | VLM（线上） |
| --- | --- | --- |
| 位于查询路径 | 是 | 否 |
| 故障影响 | 检索不可用，触发 7.1 的降级 | 仅导入受影响：摘要为空或占位符，目录级向量失效 |
| 恢复方式 | 恢复本地服务 | 恢复后对受影响目录重新索引（`/content/reindex`） |
| 成本形态 | 一次性硬件与运维 | 按导入量计费，reindex 会重复产生 |

导入与 reindex 是 VLM 成本与限流的集中点：每个文本文件一次调用、输入可达 30000 字符，再加目录级 overview。换 embedding 模型触发的全量重建也会重跑一遍摘要。因此需要为导入设置速率与预算上限，并把「一次全量重建的预计调用量与费用」在阶段 A 估出来，避免 reindex 时被限流打断。

## 8. 代码改造点

以下均为拟议改造，本文不包含任何代码变更。

| 位置 | 改造内容 | 阶段 |
| --- | --- | --- |
| 新增 `internal/openviking/` | 封装 `sdk/go@sdk/go/v0.0.2`：客户端构造、共享 `*http.Client`、错误码映射（`UNAUTHENTICATED`、`PERMISSION_DENIED`、`RESOURCE_EXHAUSTED`、`UNAVAILABLE`、`DEADLINE_EXCEEDED`，见 `utils/exceptions.py:5-19`）、重试。SDK 自身无重试，复用 `internal/model/retry.go` 的状态码表与退避 | 0 |
| 新增 `internal/knowledge/` | `Find` 封装、范围校验、预算装配、invocation 级缓存、结果结构。只保留一个 `Retriever` 接口供测试替换，不做多后端抽象 | A |
| `internal/tool/` 与 `internal/tool/metadata.go` | `knowledge_search` 与 `knowledge_read` 两个工具及其 `ops.ToolMetadata` 声明；按 `engine.go:1169-1199` 的现有方式挂载 | B |
| `internal/engine/engine.go` | `modelCallbacks`（`:1137`）里按 invocation 注入瞬态召回；OpenViking 客户端作 engine 级单例；`incidentFor`（`:937`）目前只返回默认时间窗，补真实宿主身份供 gateway 的 `PrepareArgs` 注入 | A / B |
| `internal/history/compact.go` | 注入预算与 `history.max_tokens` 对齐，召回不得挤占回答与工具调用空间 | B |
| `internal/metrics/` 与 `internal/telemetry/meters.go` | 新表 `knowledge_recalls` 用于 shadow 评估与事后追溯；新增 `RecordRecall` 指标 | A |
| `internal/storage/`、`migrations/postgres/0001_init.sql`、`internal/migrate/migrate.go:42` | 新表 `ov_knowledge_sources` 与 `knowledge_recalls`。**PostgreSQL 必须两处建表**：`ApplySchema` 明确拒绝在 PG 侧建表（`internal/storage/storage.go:256-274`），且 `TestSQLiteAndPostgresSchemasAgree` 从 `migrate.Tables` 取表名 | A |
| `internal/config/config.go` | 新增 `OpenViking` 节、`Save` 非零判断、校验（endpoint 必填、mode 枚举、roots 必须以 `viking://` 开头） | A |
| `internal/server/api.go` 等管理接口 | 导入状态、重试、撤销查询；显示降级与积压。沿用现有单管理员 cookie 鉴权（`internal/server/auth.go:94-121`），不向前端下发任何 OpenViking 密钥 | B |
| `docker-compose.yml` | 新增 profile `ov`：固定镜像 digest、命名卷 `openviking-data:/app/.openviking`、`OPENVIKING_WITH_BOT=0`、1933 只对内网、只读挂载配置、不开 `/metrics`。卷不放在 `./data` 内的理由与现有 `postgres-data` 完全相同：`./data` 被整目录挂进 Agent 容器，Agent 的文件工具会碰到底层索引 | A |
| 明确不在本轮范围 | `internal/skill/`、`internal/memory/`、`internal/platform/`、统一轮次模块、outbox 与 worker、`engine.UserID` 改造 | — |

本轮不为只读知识库重写整套 ADK `MemoryService`。现有 `BuildAgent*`、`Search()`、`NewRunner()` 对具体 `*memory.Search` 的依赖保持不动，知识能力独立接入，避免把资源检索硬塞进全文会话索引。

**顺带记录的既有缺口，本轮不修。** `internal/server/schedule.go:311-331` 从未调用 `AddSessionToMemory`，定时任务的会话从来没进过 L2 索引；而 `internal/server/chat.go:271` 与 `cmd/cli/run.go:42` 各有一份 `indexSession` 副本。`StartResultSweeper` 只在 `cmd/server/main.go:77` 接线，`jelly serve` 没接。这三条是将来做统一轮次生命周期时的既有依据。

## 9. 分阶段交付与验收

### 9.1 阶段 0：契约锁定，1 到 2 个工程日

固定镜像 digest 与 `sdk/go/v0.0.2`。契约测试以环境变量门控，仿 `JELLY_PG_DSN` 与各包的 `exclusive_for_test.go`。必须覆盖：

1. `Find` 的 `target_uri` 子树限制生效，越界目标返回 403 或空。
2. `search(mode=context)` 传 `target_uri` 返回 400。
3. api_key 模式下改 Account/User 头的实际行为（见 2.6 第 1 条）。
4. 检索命中实际返回哪些字段。
5. 导入任务的状态机与失败载荷形状。
6. `DELETE /fs` 同步删除向量。
7. `/health` 的语义与就绪判断方式。
8. 检索路径不触碰会话与 rerank 模型：确认 `Find` 不发起 intent 分析，未配置 rerank 时无重排调用。这条防的是将来有人改用 `search(session_id=...)` 而把事故对话送出网。
9. ~~长文档的尾部可检索性~~ **已完成**：导入管线会先分块，截断不影响正常文档，见 2.7。
10. VLM 不可达时的故障面（决策已定为线上 VLM，此项改为验证故障形态而非选型依据）：把 `vlm` 指向不可达端点并导入一批 Markdown，确认导入任务的终态、文件级检索是否仍可用、`abstract` 是否为空、目录 overview 是否为占位符，以及恢复后 `/content/reindex` 能否补齐摘要。这决定 7.4 那张故障面表是否成立，也决定 6.1 的退化注入路径要不要实现。
11. embedding 选型对比：在同一份语料与同一组 30 题上比较候选模型的命中率，含 `max_input_tokens` 取 4096 与 8192 的差异。必须在语料还小时完成。

任一契约与本文不符即停，不进入阶段 A。

### 9.2 阶段 A：只读 shadow

部署独立实例，导入少量脱敏运维文档到 `viking://resources/ops` 四个子目录并打 tags，来源登记落库。`mode: shadow`，不注入、不暴露工具、不改现有会话存储。

准备至少 30 个真实运维问题，覆盖已知故障、架构定位、相似但不同环境、知识缺失、过期手册、以及内含指令的恶意文档。以人工标注的相关资料为标准。

**进入阶段 B 的门槛：有答案问题的 Top 5 命中率不低于 85%。** 未达标先调数据组织与检索参数，不直接启用。

### 9.3 阶段 B：只读正式接入

`mode: on`，自动召回参与回答，通过 gateway 暴露两个只读工具，对少量授权用户灰度。完善管理端的导入状态、撤销与备份。不新增 Elasticsearch（上游也不支持），不关闭现有 L2 全文检索。

必须全部通过：

- 越权结果泄漏为零。以受限身份对未授权目录做 find 与 read，只能得到 403 或空。
- 召回预算不挤占必要的回答与工具调用空间。
- 知识服务断开后仍能正常对话与使用工具。
- 401 与 403 不触发扩权重试。
- 撤销的文档不再被引用，且缓存立即失效。
- 热更新（engine retire）与流式取消期间没有资源提前关闭。
- 文档正文里的指令不被当作系统指令执行。

排期粗估：只读技术验证约 1 到 2 个工程日；包含灰度、权限、导入管理与故障验证的正式接入约 1 到 2 周。

### 9.4 退出演练，阶段 B 上线前完成一次

1. 暂停导入与后台写入，等待正在执行的任务稳定。OVPack 是在线逐文件读取，不是原子快照，需要一致性就必须暂停写入。
2. 导出 OpenViking 内容与 Jelly 的来源登记，校验数量与哈希。`include_vectors` 默认关闭，退出计划默认接受重新向量化。
3. 在隔离环境恢复，重建必要身份与索引，验证权限与检索问题集。
4. 抽样把知识转为普通文本与 JSON 来源记录，确认不依赖 OpenViking 仍可读取核心资产。
5. 切回 `mode: off`，验证原有对话、工具与 L2 检索不受影响。

更换产品时需要重建索引并调整排序与客户端适配，不承诺零成本迁移或检索效果完全一致。

### 9.5 后续评估：不排期，只记录触发点

**Skills。** 满足任一条件才重新评估：上游为 `viking://agent/skills` 提供 ACL 或不可变版本；或业务确认愿意承担 Jelly 侧发布记录的完整成本。后者的具体形态是：发布时立即调用 `GET /api/v1/skills/{name}?include_integrity=true` 取 `revision` 与 `content_sha256` 存入发布记录（Go SDK 未封装该参数，需裸 HTTP），加载时核对不一致即 fail-closed，与上游 VikingBot 的 `SKILL_REVISION_CHANGED` 行为对齐，并把技能源码的备份责任留在 Git，因为 OVPack 不含这个根。

**长期记忆。** 前置条件四项，缺一不可：Jelly 的可信身份改造（`engine.UserID` 常量、`platform.ReplyFunc` 的发送者与群组身份、群聊不合并为个人偏好）；统一的 `Prepare / Run / Finalize` 轮次生命周期，吸收现有两份 `indexSession` 并补上定时任务入口；持久 outbox 与读回对账（上游无幂等键，网络结果不明时按 `turn_id` 读回比对，不盲目重发）；删除闭环（删会话前先按各归档的 `memory_diff.json` 收集派生记忆 URI 落库，再逐个 `DELETE /fs`，因为删会话不会删记忆）。

两者都要求上游保持 `agent_evolution` 与 `memory.session_skill_extraction_enabled` 关闭。

## 10. 待业务确认

已定事项：模型部署方式见 7.4，实施版本与端点见 2.7，阶段 A 与 B 只面向单一运维团队（因 v0.4.17.1 无 ACL，多团队分权列为需升级的后续项）。以下为仍待确认项。

1. **允许进入知识库的文档范围，以及脱敏规则与确认人。** 因 7.4 的决定，导入即等于把正文送给线上 VLM 提供商，上游无 PII 脱敏，故这是本方案最关键的一项，须在阶段 A 前定稿。
2. embedding 的自建承载：CPU 还是 GPU 主机，以及首选模型。默认建议 bge-m3，候选对比与约束见 2.5。自建 embedding 只能跑 dense 单路，sparse 与 hybrid 只有火山与 VikingDB 支持。须在阶段 A 语料还小时定稿，因为换模型等于全量重建索引，且会连带重跑全部 VLM 摘要。
3. 线上 VLM 的预算上限，含一次全量重建的预计调用量与费用（见 7.4）。提供商与模型已按现有部署确定为 `qwen3.8-flash`（provider `openai`）。
4. `/studio` 与 `/metrics` 的收口方式，以及跨主机访问是否加 TLS。当前实例为内网明文 HTTP，见 2.7。
5. 原始知识负责人、更新频率、共享案例审核人、备份保留期与 RPO / RTO 目标。上游是单实例单写者，没有高可用。

首批推进范围：单一可信运维团队、只读运维知识库、独立内网实例、embedding 自建配线上 VLM、保留现有 L2 检索、Skills 与长期记忆保持现状。
