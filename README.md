# jelly-agent

基于 [ADK-Go](https://pkg.go.dev/google.golang.org/adk) (v1.3.0) 的多模型 Agent 平台：纯 Go 单二进制，OpenAI 兼容统一接入（DeepSeek / OpenAI / Claude / Ollama …），支持流式对话与工具调用。提供 Web Dashboard 与 Terminal CLI 双入口，内置记忆、技能（Skills）、MCP，并可接入钉钉 / 个人微信等聊天平台。

## 项目状态

**Phase 4（Web 控制台）已完成，Phase 5（生产化）进行中**，并已扩展多平台消息接入。已验证可用：

- 自写 `model.LLM` OpenAI 兼容适配器（流式 + 工具调用 + DeepSeek 思考模型 `reasoning_content` 往返）。
- Agent + `web_search` 工具，端到端跑通 DeepSeek。
- 配置层（YAML + `${ENV}` + 环境变量回落）、模型 Registry、cobra 命令树。
- **交互式多轮对话** + 内联命令（`/help` `/tools` `/memory` `/clear` `/stats` `/exit`）。
- **会话持久化**：纯 Go SQLite（无 CGO），落 `~/.jelly-agent/state.db`，可列出历史会话。
- **L1 核心记忆**（Hermes 式）：`MEMORY.md` / `USER.md` 每轮注入 system prompt（带 token 预算裁剪），Agent 通过 `remember` / `forget` 工具跨会话增删长期事实。
- **L2 会话检索**（可选）：历史会话文本索引进 SQLite FTS5（与 `state.db` 同库、纯 Go trigram 分词，中英文皆可子串检索），开启后 Agent 获得 `load_memory` 工具按需检索过往对话。
- **多 Agent（协调者 + 子 Agent 转交）**：在 config / Web 定义多个具名 Agent（各自 provider、指令、MCP），给协调者挂上「子 Agent」即开启 ADK 的 `transfer_to_agent` 转交——协调者按每个子 Agent 的描述判断把任务交给谁。未定义任何 Agent 时对话仍走默认单 Agent（向后兼容）。
- **Web 控制台**：`jelly serve` 启动深色主题 Dashboard（Vue 3 + Vite，`go:embed` 进二进制），含对话、工具测试台、会话浏览（可删除）、用量监控、记忆、技能、Agent、MCP、消息绑定、Provider 配置等页面。CLI 与 Web 共用 `internal/engine` 同一运行时。
- **配置热重载**：Web 端增删改 Provider/MCP/消息绑定保存即生效；直接编辑磁盘上的 `config.yaml` 也会被监听到并自动热重载，对话不中断、无需重启。
- **技能（Skills）**：Claude/Agent Skills 风格的 Markdown 能力包，清单注入 + `use_skill` 按需加载（渐进式披露）；Web 页增删改 + 上传 ZIP 包导入。技能可附带脚本，经 `run_script` 在**沙箱**中执行。
- **沙箱执行**：脚本运行走 `internal/sandbox`，两套后端——`native`（纯 Go 零依赖、尽力而为加固：清洗环境不泄漏宿主密钥、限工作目录、超时杀整进程组、CPU 时长 + 输出截断）与可选 `docker`（强隔离：无网络、只读 rootfs、内存/PID 限额、仅挂载工作目录）；每次执行写审计日志。后端、资源上限等可在 Web **技能**页的「脚本沙箱设置」直接配置（保存即热重载），也可手动编辑 `configs/config.example.yaml` 的 `sandbox` 段。
- **消息绑定（多平台）**：把同一套 Agent 接入聊天平台，纯本地无需公网。
  - **钉钉**：官方 Stream 模式（出站 WebSocket）；可绑定 AI 卡片模板实现**流式回复**。
  - **个人微信**：经 WeChatPadPro 网关（iPad 协议）接入，Web 页扫码登录，文本收发（⚠️ 第三方协议有封号风险）。
  - 每个机器人可**选择应答 Provider 与按需加载的 MCP**。

> 详细实施依据见 `PLAN.md`（本地文档，不入库）。本 README 是仓库内唯一受 git 追踪的文档。

## 环境要求

- **Go 1.25+**（开发机 1.26；ADK-Go v1.3.0 要求 ≥1.25）
- 一个 OpenAI 兼容模型的 API Key（如 DeepSeek）

## 快速开始

```bash
# 构建
go build -o jelly ./cmd/cli

# 方式一：环境变量（最简单）
export LLM_API_KEY=sk-xxxx LLM_MODEL=deepseek-chat
./jelly agent run                       # 进入交互式多轮对话
./jelly agent run --once "用一句话介绍你自己"   # 单轮问答后退出

# 方式二：配置文件
cp configs/config.example.yaml configs/config.yaml   # 按需修改，api_key 用 ${ENV}
export DEEPSEEK_API_KEY=sk-xxxx
./jelly agent run
```

配置查找顺序：`--config` 指定 → `$JELLY_CONFIG` → `configs/config.yaml` → `~/.jelly-agent/config.yaml` → 回落到 `LLM_API_KEY` / `LLM_BASE_URL` / `LLM_MODEL` 环境变量。

## 命令

```text
jelly agent run                        # 交互式多轮对话（默认）
jelly agent run --once "问题"          # 单次问答后退出
jelly agent list                       # 列出 Agent
jelly session list                     # 列出持久化的历史会话
jelly config list                      # 列出 Provider（API Key 脱敏）
jelly tool list                        # 列出内置工具
jelly serve                            # 启动 Web 控制台（默认 :6185）
jelly --help                           # 全部命令
```

交互模式内联命令：`/help` `/tools` `/memory`（查看长期记忆）`/clear`（开新会话）`/stats`（token 用量）`/exit`（或 Ctrl+D）。

可选环境变量：`TAVILY_API_KEY`（设置后 `web_search` 走 Tavily，否则回落免 key 的 DuckDuckGo）。

## 核心记忆（L1）

Agent 维护两份 markdown 长期记忆，每轮对话自动拼进 system prompt（各有 token 预算，超限自动裁剪最旧条目）：

- `~/.jelly-agent/memory/USER.md` —— 用户画像（默认上限 500 token）
- `~/.jelly-agent/memory/MEMORY.md` —— 代理长期笔记（默认上限 800 token）

两种写入方式：① 模型在对话中通过 `remember` / `forget` 工具自行增删条目（仅当出现值得跨会话记住的偏好/身份/约定时）；② 在 Web **记忆**页直接编辑 USER.md / MEMORY.md（保存即生效，下一轮对话注入）。目录与预算可在 `config.yaml` 的 `memory.core` 段覆盖。`/memory` 命令随时查看当前内容。

## 技能（Skills）

参考 Claude/Anthropic Agent Skills：每个技能是一份 Markdown（`name` + `description` + 详细指令）。Agent 平时只看到**技能清单**（名称 + 描述）注入到 system prompt，命中某项需求时调用 `use_skill` 工具拉取该技能**全文**再按步骤执行——渐进式披露，技能再多也不撑爆基础 prompt。

- 在 Web **技能**页增删改：填 `name`（标识符，`[A-Za-z0-9_-]+`）、`description`（进清单）、正文（Markdown）、启停；也可**上传 ZIP 包**导入。
- 两种磁盘布局：手填的存为 `~/.jelly-agent/skills/<name>.md`；ZIP 导入的解压到 `~/.jelly-agent/skills/<name>/`（含 `SKILL.md` 与随包资源文件）。均可直接编辑，下一轮对话自动生效（无需重启）。
- 仅启用的技能进清单；正文只在 `use_skill` 调用时返回。

**ZIP 导入**：zip 内须有一个 `SKILL.md`（在根目录或某个顶层文件夹里），其 frontmatter 的 `name` 作为技能标识；该文件夹内的其它文件作为随包资源一并解压（带防 zip-slip 与大小限制）。导入后默认启用。

**变量（密钥）**：在技能编辑里给技能配置变量（`KEY=VALUE`）。值**脱敏存储**——存进 `config.yaml`（`0600`，**不进技能文件**，所以分享/导出技能不带密钥），支持 `${ENV}` 引用，Web 只回显键名、不回显值。

**脚本执行（默认关闭）**：技能页顶部「允许脚本执行」开关打开后，Agent 可调 `run_script` 运行该技能目录下的脚本（`.sh`/`.py`/`.js` 或可执行文件），技能变量作为**环境变量**注入脚本；脚本工作目录限定在技能目录内（防越界）、带超时与输出截断。
- 密钥**只进子进程环境**，不进 system prompt、不进 `use_skill` 输出、不进会话记录——脚本自己别把密钥打印出来即可。
- ⚠️ 它以你的权限执行代码，**不是沙箱**；仅对自己信任的技能开启。

```markdown
---
name: weekly-report
description: 把本周事项整理成结构化中文周报
enabled: true
---
## 步骤
1. 收集本周事项 …
```

## 会话检索（L2，可选）

设 `memory.search.enabled: true` 后开启——既可改 `config.yaml`，也可在 Web **记忆**页右上角的开关一键启停（保存即写入配置并热重载，即时生效、无需重启）。开启后每轮结束把会话文本写入 `state.db` 内的 FTS5 全文索引，Agent 据此获得 `load_memory` 工具，可在后续会话里检索过往对话（返回 top-K，永不回灌整段历史）。采用 trigram 分词，中英文均按子串匹配（查询需 ≥3 字符，更短自动回落 LIKE）。配置项见 `memory.search`（`top_k` 等）。向量语义检索（L3）属后续。

## Web 控制台

面向开发者的测试台（Vue 3 + Vite，深色主题，`go:embed` 打包进二进制）。页面：

- **对话** —— SSE 流式响应、工具调用过程可视化、逐轮 Token 统计；多 Provider 下拉切换（记住上次选择、标注默认项，多 Provider 时为每条回复标注应答模型）。定义了多 Agent 后多出 Agent 下拉（可选「单 Agent（默认）」回落），发生转交时气泡标注当前应答的子 Agent。
- **工具** —— 列出内置工具，并内置 `web_search` 测试台（绕过模型直接调用）。
- **会话** —— 列出持久化历史会话，查看完整 transcript。
- **监控** —— 跨全部持久化会话聚合用量：会话/消息/工具调用/Token 总量 KPI、Token 构成、工具调用排行、每日 Token 趋势柱状图。
- **记忆** —— L1 核心记忆（USER.md / MEMORY.md）快照 + L2 FTS5 会话全文检索。
- **Agent** —— 定义具名 Agent（provider / 描述 / 系统指令 / MCP / 子 Agent），新建/编辑/启停/删除、设默认；给协调者勾选子 Agent 即组成转交树。详见下方「多 Agent」。
- **MCP** —— 接入外部 Model Context Protocol 服务器（stdio / http / sse），新建/编辑/启停/删除、一键测试连接并列出其工具；启用后其工具与内置工具一起注入 Agent。
- **消息绑定** —— 把钉钉接入为消息入口：新建/编辑/启停/删除钉钉机器人，实时显示连接状态（在线/连接中/错误），启用即连接、无需重启。详见下方「消息绑定」。
- **配置** —— 在线增删改 Provider（OpenAI 兼容端点）、设默认，保存即**热重载**，无需重启。

配置（Provider / MCP / 消息绑定）写入 `configs/config.yaml` 或 `~/.jelly-agent/config.yaml`（`0600`）；API Key 与各类密钥脱敏展示，编辑时留空即保留原值，`${ENV}` 引用不会被改写成明文。直接编辑该文件也会被服务器监听并热重载，无需重启。

## 多 Agent（协调者 + 子 Agent 转交）

把单个 Agent 扩展成一棵可委派的 Agent 树：一个**协调者**根据子 Agent 的描述，按需把整轮对话**转交**给最合适的专家（ADK 的 `transfer_to_agent` 委派，由协调者的 LLM 自行决定）。

- 每个 Agent 是一条具名定义：`name`（标识符）、`description`（**供上级判断何时转交，务必写清职责**）、`provider`（留空=默认 Provider，可让不同 Agent 跑不同模型）、`instruction`（系统指令，留空=内置默认）、`mcp`（按需加载的 MCP 子集，不选=不挂）、`sub_agents`（可转交的子 Agent 名）、`enabled`。
- 在 Web **Agent** 页增删改：先建若干专家，再建协调者并勾选其子 Agent、设为默认；也可直接写 `config.yaml` 的 `agents` / `default_agent` 段。保存即热重载。
- 对话时在「对话」页顶部选 Agent（或用 `default_agent`）；选「单 Agent（默认）」则回落到旧的单 Agent + Provider 模式。发生转交时气泡会标注当前应答的子 Agent。
- 安全约束：拒绝自引用、引用不存在的子 Agent；删除某 Agent 时自动从其它 Agent 的子列表与 `default_agent` 中清除；构树带环检测与深度上限。

```yaml
default_agent: coordinator
agents:
  - name: coordinator
    description: 总协调，按子 Agent 职责决定转交给谁
    provider: deepseek          # 留空=默认 Provider
    instruction: 你是协调者，先判断该由哪个专家处理，再决定是否转交。
    sub_agents: [researcher, coder]
    enabled: true
  - name: researcher
    description: 擅长联网检索与资料整理
    mcp: [filesystem]           # 不选=不挂 MCP
    enabled: true
  - name: coder
    description: 写代码与调试
    enabled: true
```

> 未定义任何 `agents` 时，CLI / Web / 消息绑定一切照旧走默认单 Agent——多 Agent 是纯增量、向后兼容。

## 消息绑定

把外部聊天平台接入同一套 Agent（含记忆 / 工具 / MCP）。已支持**钉钉**与**个人微信**，均**无需公网**，纯本地即可用。会话键 `"<平台>-<会话ID>"` 持久化，跨消息保留多轮上下文。

每个机器人可**选择性加载 MCP**：在「消息绑定」表单里勾选该机器人要加载的 MCP 服务器（不勾=不加载），避免把所有 MCP 工具都塞给每个机器人。（Web「对话」页仍加载全部已启用的 MCP。）

### 钉钉

在钉钉里 @机器人 发消息，jelly-agent 应答并回到钉钉。采用钉钉官方 **Stream 模式（出站 WebSocket）**，**无需公网 URL / 域名 / IP**。

接入步骤：
1. 钉钉开放平台建「企业内部应用」→ 机器人，开启 **Stream 模式**，拿到 ClientID（AppKey）与 ClientSecret（AppSecret）。
2. （可选，流式回复）在钉钉「卡片平台」建一个 **AI 卡片模板**，模板里放一个名为 `content` 的流式文本组件，拿到**卡片模板 ID**。
3. `jelly serve` 启动控制台 → 「消息绑定」页 → 新建钉钉机器人，填凭据（可选填卡片模板 ID）并启用 → 状态徽标变「在线」。
4. 钉钉群 @机器人 提问即可；同一会话（`sessionID = "dingtalk-" + 钉钉会话ID`）跨消息保留多轮上下文，并随 L2 检索可被回忆。

**流式回复**：填了卡片模板 ID 后，回复会以钉钉 **AI 卡片**逐字流式呈现（创建卡片 → 随生成增量更新 → 收尾）；不填则回退为单条 Markdown 文本。

配置段示例（亦可直接写 `config.yaml`，密钥支持 `${ENV}`）：

```yaml
platforms:
  - name: my-dingtalk
    type: dingtalk
    enabled: true
    client_id: ${DINGTALK_CLIENT_ID}
    client_secret: ${DINGTALK_CLIENT_SECRET}
    provider: ""   # 留空=默认 Provider
    settings:
      card_template_id: ""   # 填卡片模板 ID 即启用流式 AI 卡片回复
```

### 个人微信（WeChatPadPro）

> ⚠️ **风险提示**：个人微信自动化走第三方 iPad 协议（参考 AstrBot/LangBot），**违反微信使用条款，有封号风险**，强烈建议用小号。

个人微信无官方协议，需自建第三方协议网关 **WeChatPadPro**（gewechat 已停更）。jelly-agent 连接它的 HTTP + WebSocket，**纯本地、无需公网**；当前支持**文本**收发（媒体后续）。

接入步骤：
1. 用 Docker 自建 [WeChatPadPro](https://github.com/WeChatPadPro/WeChatPadPro)，记下其 HTTP 地址（如 `http://127.0.0.1:9090`）、WebSocket 地址与 `admin_key`。
2. `jelly serve` → 「消息绑定」页 → 新建「个人微信（WeChatPadPro）」，填 `wechatpad_url` / `wechatpad_ws` / `admin_key` 并启用。
3. 页面出现**登录二维码** → 手机微信「扫一扫」登录 → 状态徽标变「在线」。
4. 给该微信发消息（群里需 @机器人）即可；会话键 `"wechat-" + 微信会话ID`。

配置段示例：

```yaml
platforms:
  - name: my-wechat
    type: wechatpadpro
    enabled: true
    settings:
      wechatpad_url: http://127.0.0.1:9090
      wechatpad_ws: ws://127.0.0.1:9090/ws
      admin_key: ${WECHATPAD_ADMIN_KEY}
    provider: ""   # 留空=默认 Provider
```

> 企业微信 / 公众号（需公网回调）列入后续批次。

```bash
# 生产：先构建前端，再编译进二进制，单文件部署
cd web && npm install && npm run build && cd ..
go build -o jelly ./cmd/cli
./jelly serve                          # 打开 http://localhost:6185

# 也可用独立服务器入口（等价）：go build ./cmd/server && ./server

# 前端开发：Vite 热更新 + 代理到本地 Go 服务
go run ./cmd/cli serve &               # 后端 :6185
cd web && npm run dev                  # 前端 :5273，/api 自动代理
```

未构建前端时 `jelly serve` 仍可运行（仅 API）；REST 接口见 `internal/server`（`/api/chat/stream` 为 SSE，其余为 JSON）。

## 测试

```bash
go test ./...    # 离线单测（含适配器 genai⇄openai 转换、reasoning 往返、server API 表层）
go vet ./...
```

## 目录

```text
cmd/cli/              # CLI 入口（cobra 命令树 + 交互式 REPL + serve 子命令）
cmd/server/           # 独立 Web 服务器入口
internal/engine/      # 运行时装配（model/agent/runner/session/memory + 多 Agent 转交树），CLI 与 server 共用
internal/server/      # REST + SSE 处理器 + SPA 静态服务 + Provider/MCP/Agent/沙箱 配置热重载
internal/mcp/         # MCP 接入（stdio/http/sse transport + toolset + 直连列举）
internal/model/       # OpenAI 兼容 model.LLM 适配器 + Registry
internal/tool/        # 内置工具（web_search、remember/forget、load_memory）
internal/config/      # YAML + ${ENV} 配置加载
internal/memory/      # L1 核心记忆（MEMORY/USER.md）+ L2 FTS5 会话检索
internal/session/     # SQLite 会话持久化（纯 Go，无 CGO）
web/                  # Vue 3 + Vite 前端源码（go:embed dist）
configs/              # 配置示例
```

## 许可

待定。
