# jelly-agent

基于 [ADK-Go](https://pkg.go.dev/google.golang.org/adk) (v1.3.0) 的多模型 Agent 平台：纯 Go 单二进制，OpenAI 兼容统一接入（DeepSeek / OpenAI / Claude / Ollama …），支持流式对话与工具调用，规划中提供 Web Dashboard 与 Terminal CLI 双入口。

## 项目状态

当前处于 **Phase 2（CLI 完善）· 第四批：记忆地基**。已验证可用：

- 自写 `model.LLM` OpenAI 兼容适配器（流式 + 工具调用 + DeepSeek 思考模型 `reasoning_content` 往返）。
- 单 Agent + `web_search` 工具，端到端跑通 DeepSeek。
- 配置层（YAML + `${ENV}` + 环境变量回落）、模型 Registry、cobra 命令树。
- **交互式多轮对话** + 内联命令（`/help` `/tools` `/memory` `/clear` `/stats` `/exit`）。
- **会话持久化**：纯 Go SQLite（无 CGO），落 `~/.jelly-agent/state.db`，可列出历史会话。
- **L1 核心记忆**（Hermes 式）：`MEMORY.md` / `USER.md` 每轮注入 system prompt（带 token 预算裁剪），Agent 通过 `remember` / `forget` 工具跨会话增删长期事实。
- **L2 会话检索**（可选）：历史会话文本索引进 SQLite FTS5（与 `state.db` 同库、纯 Go trigram 分词，中英文皆可子串检索），开启后 Agent 获得 `load_memory` 工具按需检索过往对话。

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

在 `config.yaml` 设 `memory.search.enabled: true` 后开启：每轮结束把会话文本写入 `state.db` 内的 FTS5 全文索引，Agent 据此获得 `load_memory` 工具，可在后续会话里检索过往对话（返回 top-K，永不回灌整段历史）。采用 trigram 分词，中英文均按子串匹配（查询需 ≥3 字符，更短自动回落 LIKE）。配置项见 `memory.search`（`top_k` 等）。向量语义检索（L3）属后续。

## 测试

```bash
go test ./...    # 离线单测（含适配器 genai⇄openai 转换、reasoning 往返）
go vet ./...
```

## 目录

```text
cmd/cli/              # CLI 入口（cobra 命令树 + 交互式 REPL）
internal/model/       # OpenAI 兼容 model.LLM 适配器 + Registry
internal/tool/        # 内置工具（web_search、remember/forget、load_memory）
internal/config/      # YAML + ${ENV} 配置加载
internal/memory/      # L1 核心记忆（MEMORY/USER.md）+ L2 FTS5 会话检索
internal/session/     # SQLite 会话持久化（纯 Go，无 CGO）
configs/              # 配置示例
```

## 许可

待定。
