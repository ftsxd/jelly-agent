# Agent 自主查询接入

执行生命周期可用并不意味着每个 Agent 自动获得执行器。查询 Agent 必须获明确分配、拥有变量映射，并能在隔离环境中加载 CLI。仅向演示 Agent 分配执行器，无法使 TencentQuery 实际调用腾讯云。

## 配置腾讯云查询 Agent

1. 在 Agent 变量页保存 `TENCENTCLOUD_SECRET_ID`、`TENCENTCLOUD_SECRET_KEY`、`TENCENTCLOUD_REGION`。值由服务端保存，不进入模型上下文；生产诊断应使用只读 IAM 凭据。
2. 在工具页启用执行器，明确分配给 TencentQuery，启用联网，引用该 Agent 的三个变量名称。不要把密钥写进提示词或命令。给协调者和 TencentQuery 的职责描述加入 CLS 日志集/主题元数据查询。
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
      agent_env:
        TENCENTCLOUD_SECRET_ID: TENCENTCLOUD_SECRET_ID
        TENCENTCLOUD_SECRET_KEY: TENCENTCLOUD_SECRET_KEY
        TENCENTCLOUD_REGION: TENCENTCLOUD_REGION
      write_approval: true
```

`agent_env` 的值是当前 Agent 的变量名称；`env` 则引用服务端进程环境变量名称。不会自动继承其他 Agent 的变量或宿主凭据。CLS Describe/List/SearchLog 与 CLI 帮助有内置只读规则；其他产品需要在对应 profile 明确添加规则，资源访问权限仍由 IAM 限制。

## 自主执行与审批

已分配的 shell_exec 自动成为该节点必需工具，工具预算不会在简短追问中把它丢掉。系统提示要求实际查询，参数不确定先查 help，参数错误返回模型后修正，核对总数并分页。大量返回应使用 CLI 原生 `--filter` 保留总数、请求标识和所需字段；截断的返回无法证明清单完整。权限失败应报告真实证据。

写操作需要逐条确认，十分钟失效，仅执行一次；模型无法通过自然语言同意绕过审批。可用 `write_env` 或 `write_agent_env` 映射独立写凭据。审批专用 Agent 来源不能同时供自动查询使用，也不会注入技能脚本，即使临时关闭执行器仍保留这一限制。改动已引用变量会使原审批失效。

`tool_dir` 仅增加 bin/lib/lib64/pyvenv.cfg 的读取权限，保留隔离 HOME；这些入口的链接不得指向运行目录之外。模型不能指定宿主路径、环境、工作目录或执行其他 Agent 的 profile。

## 验证

`engine/execution_loop_test.go` 使用模拟模型与本地 CLI 走通转交、帮助、参数错误、修正查询、结果证据五步，在工具预算 1 下仍保留执行能力。该测试不调用云业务 API；真实模型的行动和结果还需要在已配置环境中验证。

此次本地体验配置与日志存放在忽略的 `data/runtime-preview`，包含私有状态和凭据，禁止提交。其他执行生命周期限制见 [验证报告](execution-lifecycle-verification.md)。
