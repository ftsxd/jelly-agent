# jelly-agent

基于 [ADK-Go](https://pkg.go.dev/google.golang.org/adk) (v1.3.0) 的多模型 Agent 平台：纯 Go 单二进制，OpenAI 兼容统一接入（DeepSeek / OpenAI / Claude / Ollama …），支持流式对话与工具调用，规划中提供 Web Dashboard 与 Terminal CLI 双入口。

## 项目状态

当前处于 **Phase 2（CLI 完善）· 第一批：打地基**。已验证可用：

- 自写 `model.LLM` OpenAI 兼容适配器（流式 + 工具调用 + DeepSeek 思考模型 `reasoning_content` 往返）。
- 单 Agent + `web_search` 工具，端到端跑通 DeepSeek。
- 配置层（YAML + `${ENV}` + 环境变量回落）、模型 Registry、cobra 命令树骨架。

> 详细实施依据见 `PLAN.md`（本地文档，不入库）。本 README 是仓库内唯一受 git 追踪的文档。

## 环境要求

- **Go 1.25+**（开发机 1.26；ADK-Go v1.3.0 要求 ≥1.25）
- 一个 OpenAI 兼容模型的 API Key（如 DeepSeek）

## 快速开始

```bash
# 构建
go build -o jelly ./cmd/cli

# 方式一：环境变量（最简单）
LLM_API_KEY=sk-xxxx LLM_MODEL=deepseek-chat \
  ./jelly agent run root --once "用一句话介绍你自己"

# 方式二：配置文件
cp configs/config.example.yaml configs/config.yaml   # 按需修改，api_key 用 ${ENV}
export DEEPSEEK_API_KEY=sk-xxxx
./jelly agent run root --once "搜索一下 2026 年 Go 在 AI 领域的最新进展"
```

配置查找顺序：`--config` 指定 → `$JELLY_CONFIG` → `configs/config.yaml` → `~/.jelly-agent/config.yaml` → 回落到 `LLM_API_KEY` / `LLM_BASE_URL` / `LLM_MODEL` 环境变量。

## 命令

```text
jelly agent run [root] --once "问题"   # 单次问答（交互式多轮见第二批）
jelly agent list                       # 列出 Agent
jelly config list                      # 列出 Provider（API Key 脱敏）
jelly tool list                        # 列出内置工具
jelly --help                           # 全部命令
```

可选环境变量：`TAVILY_API_KEY`（设置后 `web_search` 走 Tavily，否则回落免 key 的 DuckDuckGo）。

## 测试

```bash
go test ./...    # 离线单测（含适配器 genai⇄openai 转换、reasoning 往返）
go vet ./...
```

## 目录

```text
cmd/cli/              # CLI 入口（cobra 命令树）
internal/model/       # OpenAI 兼容 model.LLM 适配器 + Registry
internal/tool/        # 内置工具（web_search）
internal/config/      # YAML + ${ENV} 配置加载
configs/              # 配置示例
```

## 许可

待定。
