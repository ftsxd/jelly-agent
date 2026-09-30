# Agent 自主查询接入

执行生命周期可用并不意味着每个 Agent 自动获得执行器。查询 Agent 必须获明确分配、拥有变量映射，并能在隔离环境中加载 CLI。仅向演示 Agent 分配执行器，无法使 TencentQuery 实际调用腾讯云。

## 换环境部署

拉取代码不会迁移数据库、启用执行配置、复制 Agent 私有变量或安装 tccli。本地体验的 `data/runtime-preview` 没有提交；另一台机器要按下节重新配置。

如果工具页整块“通用诊断执行器”卡片没有出现，先检查运行镜像/前端版本。当前源码无条件渲染这块卡片，即使未启用或接口报错也会显示标题。此前 Compose 只有 `image: jelly-agent:local`，拉取源码再重启会继续运行旧镜像；现已加入源码构建配置。`web/dist` 不提交，Go 二进制内嵌的是编译时的前端；Dockerfile 会重新构建前端后再编译服务。

在已更新源码的仓库根目录执行：

```sh
JELLY_BUILD_REVISION=$(git rev-parse HEAD) docker compose up -d --build jelly-agent
docker image inspect jelly-agent:local --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}'
```

镜像标签应该与本次源码提交一致。然后强制刷新浏览器，再打开工具页。`.dockerignore` 排除了本地配置、数据、密钥环境文件和旧前端产物，避免把本地体验状态带进构建镜像。使用独立前端/反向代理部署时，还需同步其前端产物；不能仅更新后端镜像。

已有 PostgreSQL 库先应用 `migrations/postgres/0003_execution_runtime_upgrade.sql`，补齐审批和执行恢复表后重启服务。不要在旧库重跑整份 `0001_init.sql`。具体命令和数据库/schema 选择见 [迁移说明](../migrations/README.md)。

如果仍然只返回脚本，在工具页确认执行器已启用且 profile 分配给实际接收任务的 TencentQuery；查看该次工具调用是否包含 `shell_exec`。没有调用记录时，不能把脚本当成执行结果。还需确认变量名称映射，以及隔离环境能加载的 CLI 安装。Linux 容器部署建议使用预装 CLI 的诊断镜像，宿主机安装不会自动进入容器；`tool_dir` 只适用于系统沙箱。

仓库主服务 `Dockerfile` 带 Python/git，但没有 tccli 或 Docker 客户端；`docker-compose.yml` 也未给主服务接入 Docker 服务。`images/sre-runtime/Dockerfile` 是单独的诊断镜像。要让容器中的主服务使用 Docker 执行后端，需由部署方配置 Docker 客户端和服务连接，并让主服务私有工作目录在 Docker 服务所在主机上使用同一绝对路径、访问权限一致。仅挂载 `./data:/data` 不能证明两边路径一致；不能假设应用容器内部的 `/tmp` 是宿主机的 `/tmp`。这些部署条件未满足时不会自动降级执行。

## 配置腾讯云查询 Agent

1. 在 Agent 变量页保存 `TENCENTCLOUD_SECRET_ID`、`TENCENTCLOUD_SECRET_KEY`、`TENCENTCLOUD_REGION`。值由服务端保存，不进入模型上下文；生产诊断应使用只读 IAM 凭据。
2. 在 Agent 页编辑 TencentQuery，在「诊断执行」里勾选「为此 Agent 新建执行配置」并允许联网（或在工具页分配已有配置）。保存的三个变量会按同名自动注入，无需再逐个引用；注意不要填进「服务端环境变量」，那一类读取的是服务进程自己的环境变量。不要把密钥写进提示词或命令。给协调者和 TencentQuery 的职责描述加入 CLS 日志集/主题元数据查询。
3. 预装 CLI 到隔离环境可读的位置。用户 Python 安装依赖真实 HOME，不能直接用于隔离执行；可用已有安装准备独立运行目录：

```sh
python3 scripts/prepare-tccli-runtime.py --dest /absolute/path/tencent-cli
```

使用已安装 tccli 的 Python 执行此准备脚本。脚本仅复制已安装的 CLI 及必需依赖，不联网安装，不复制用户配置或凭据；目的目录须为空。生产 Linux 也可使用预装工具的 Docker 镜像，此时不配置 `tool_dir`。

```yaml
execution:
  enabled: true
  backend: os
  timeout_sec: 30
  max_output_kb: 64
  profiles:
    - name: tencent-readonly
      agents: [TencentQuery]
      network: true
      tool_dir: /absolute/path/tencent-cli
      write_approval: true
```

分配的 Agent 在变量页保存的变量默认按同名注入（`inherit_agent_vars` 不写即开启；审批专用的 `write_agent_env` 来源、会改变执行环境的变量名除外）。只有需要改名时才写 `agent_env`，它的值是当前 Agent 的变量名称；`env` 则引用服务端进程环境变量名称。不会自动继承其他 Agent 的变量或宿主凭据。CLS Describe/List/SearchLog 与 CLI 帮助有内置只读规则；其他产品需要在对应 profile 明确添加规则，资源访问权限仍由 IAM 限制。

## 自主执行与审批

已分配的 shell_exec 自动成为该节点必需工具，工具预算不会在简短追问中把它丢掉。系统提示要求实际查询，参数不确定先查 help，参数错误返回模型后修正，核对总数并分页。大量返回应使用 CLI 原生 `--filter` 保留总数、请求标识和所需字段；截断的返回无法证明清单完整。权限失败应报告真实证据。

写操作需要逐条确认，十分钟失效，仅执行一次；模型无法通过自然语言同意绕过审批。可用 `write_env` 或 `write_agent_env` 映射独立写凭据。审批专用 Agent 来源不能同时供自动查询使用，也不会注入技能脚本，即使临时关闭执行器仍保留这一限制。改动已引用变量会使原审批失效。

`tool_dir` 仅增加 bin/lib/lib64/pyvenv.cfg 的读取权限，保留隔离 HOME；这些入口的链接不得指向运行目录之外。模型不能指定宿主路径、环境、工作目录或执行其他 Agent 的 profile。

## 验证

`engine/execution_loop_test.go` 使用模拟模型与本地 CLI 走通转交、帮助、参数错误、修正查询、结果证据五步，在工具预算 1 下仍保留执行能力。该测试不调用云业务 API；真实模型的行动和结果还需要在已配置环境中验证。

此次本地体验配置与日志存放在忽略的 `data/runtime-preview`，包含私有状态和凭据，禁止提交。其他执行生命周期限制见 [验证报告](execution-lifecycle-verification.md)。
