# jelly-agent

基于 [ADK-Go](https://pkg.go.dev/google.golang.org/adk) (v1.3.0) 的多模型 Agent 平台：纯 Go 单二进制，OpenAI 兼容统一接入（DeepSeek / OpenAI / Claude / Ollama …），支持流式对话与工具调用，规划中提供 Web Dashboard 与 Terminal CLI 双入口。

## 项目状态

当前处于 **Phase 2（CLI 完善）→ Phase 4（Web 控制台）**。已验证可用：

- 自写 `model.LLM` OpenAI 兼容适配器（流式 + 工具调用 + DeepSeek 思考模型 `reasoning_content` 往返）。
- 单 Agent + `web_search` 工具，端到端跑通 DeepSeek。
- 配置层（YAML + `${ENV}` + 环境变量回落）、模型 Registry、cobra 命令树。
- **交互式多轮对话** + 内联命令（`/help` `/tools` `/memory` `/clear` `/stats` `/exit`）。
- **会话持久化**：纯 Go SQLite（无 CGO），落 `~/.jelly-agent/state.db`，可列出历史会话。
- **L1 核心记忆**（Hermes 式）：`MEMORY.md` / `USER.md` 每轮注入 system prompt（带 token 预算裁剪），Agent 通过 `remember` / `forget` 工具跨会话增删长期事实。
- **L2 会话检索**（可选）：历史会话文本索引进 SQLite FTS5（与 `state.db` 同库、纯 Go trigram 分词，中英文皆可子串检索），开启后 Agent 获得 `load_memory` 工具按需检索过往对话。
- **Web 控制台**：`jelly serve` 启动深色主题 Dashboard（Vue 3 + Vite，`go:embed` 进二进制），含对话（SSE 流式 + 工具可视化）、工具测试台、会话浏览、记忆查看、用量监控、MCP 与 Provider 配置等页面。CLI 与 Web 共用 `internal/engine` 同一运行时。
- **配置热重载**：Web 端增删改 Provider/MCP 保存即生效；直接编辑磁盘上的 `config.yaml`（编辑器、`git pull`、配置管理工具）也会被监听到并自动热重载，对话不中断、无需重启。

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

模型在对话中通过 `remember` / `forget` 工具自行增删条目；目录与预算可在 `config.yaml` 的 `memory.core` 段覆盖。`/memory` 命令随时查看当前内容。

## 会话检索（L2，可选）

设 `memory.search.enabled: true` 后开启——既可改 `config.yaml`，也可在 Web **记忆**页右上角的开关一键启停（保存即写入配置并热重载，即时生效、无需重启）。开启后每轮结束把会话文本写入 `state.db` 内的 FTS5 全文索引，Agent 据此获得 `load_memory` 工具，可在后续会话里检索过往对话（返回 top-K，永不回灌整段历史）。采用 trigram 分词，中英文均按子串匹配（查询需 ≥3 字符，更短自动回落 LIKE）。配置项见 `memory.search`（`top_k` 等）。向量语义检索（L3）属后续。

## Web 控制台

面向开发者的测试台（Vue 3 + Vite，深色主题，`go:embed` 打包进二进制）。页面：

- **对话** —— SSE 流式响应、工具调用过程可视化、逐轮 Token 统计；多 Provider 下拉切换（记住上次选择、标注默认项，多 Provider 时为每条回复标注应答模型）。
- **工具** —— 列出内置工具，并内置 `web_search` 测试台（绕过模型直接调用）。
- **会话** —— 列出持久化历史会话，查看完整 transcript。
- **监控** —— 跨全部持久化会话聚合用量：会话/消息/工具调用/Token 总量 KPI、Token 构成、工具调用排行、每日 Token 趋势柱状图。
- **记忆** —— L1 核心记忆（USER.md / MEMORY.md）快照 + L2 FTS5 会话全文检索。
- **MCP** —— 接入外部 Model Context Protocol 服务器（stdio / http / sse），新建/编辑/启停/删除、一键测试连接并列出其工具；启用后其工具与内置工具一起注入 Agent。
- **消息绑定** —— 把钉钉接入为消息入口：新建/编辑/启停/删除钉钉机器人，实时显示连接状态（在线/连接中/错误），启用即连接、无需重启。详见下方「消息绑定」。
- **配置** —— 在线增删改 Provider（OpenAI 兼容端点）、设默认，保存即**热重载**，无需重启。

配置（Provider / MCP / 消息绑定）写入 `configs/config.yaml` 或 `~/.jelly-agent/config.yaml`（`0600`）；API Key 与各类密钥脱敏展示，编辑时留空即保留原值，`${ENV}` 引用不会被改写成明文。直接编辑该文件也会被服务器监听并热重载，无需重启。

## 消息绑定（钉钉）

把钉钉作为消息入口接入同一套 Agent（含记忆 / 工具 / MCP）：在钉钉里 @机器人 发消息，jelly-agent 应答并回到钉钉。采用钉钉官方 **Stream 模式（出站 WebSocket）**，**无需公网 URL / 域名 / IP**，纯本地单二进制即可用。

接入步骤：
1. 钉钉开放平台建「企业内部应用」→ 机器人，开启 **Stream 模式**，拿到 ClientID（AppKey）与 ClientSecret（AppSecret）。
2. `jelly serve` 启动控制台 → 「消息绑定」页 → 新建钉钉机器人，填凭据并启用 → 状态徽标变「在线」。
3. 钉钉群 @机器人 提问即可；同一会话（`sessionID = "dingtalk-" + 钉钉会话ID`）跨消息保留多轮上下文，并随 L2 检索可被回忆。

配置段示例（亦可直接写 `config.yaml`，密钥支持 `${ENV}`）：

```yaml
platforms:
  - name: my-dingtalk
    type: dingtalk
    enabled: true
    client_id: ${DINGTALK_CLIENT_ID}
    client_secret: ${DINGTALK_CLIENT_SECRET}
    provider: ""   # 留空=默认 Provider
```

> 微信（企业微信 / 公众号）需公网回调地址，列入后续批次。

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
internal/engine/      # 运行时装配（model/agent/runner/session/memory），CLI 与 server 共用
internal/server/      # REST + SSE 处理器 + SPA 静态服务 + Provider/MCP 配置热重载
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
