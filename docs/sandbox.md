# 脚本沙箱

`run_script` 执行的是半可信代码——技能可以从 zip 导入、在 Web 表单里编辑，极端情况下由模型自己写出来。这份文档说明它被关在什么里面，以及**哪些地方没关住**。

## 一句话模型：mode

日常只需要配 `sandbox.mode` 一项，后端交给自动选择：

| mode | 写 | 网络 | 什么时候用 |
|---|---|---|---|
| `read-only` | 不能写任何文件 | 断 | 只读诊断：查状态、拉指标、出报告 |
| `workspace`（默认） | 只能写技能自己的目录 | 断 | 需要落中间文件的脚本 |
| `workspace-net` | 只能写技能自己的目录 | 通 | 要查 Prometheus / K8s API / 内网接口——**运维脚本多数落在这一档** |
| `full` | 不限 | 通 | 逃生舱。每次执行都会在审计里标成「未施加隔离」 |

读取在所有 mode 下都限定在「系统目录 + 技能自己的目录 + `read_paths` 列出的路径」。agent 自己的配置和状态（API key、会话、SQLite）在这个范围之外，脚本读不到。

这套三档抽象抄的是 Codex CLI 的 `read-only / workspace-write / danger-full-access`：一个词表达意图，比九个旋钮好用，也好审。

## 三个后端

| 后端 | 是什么 | 代价 |
|---|---|---|
| `native` | 纯 Go 加固：清洗环境、限工作目录、超时杀整进程组、CPU 上限、输出截断 | 零依赖，但**不是安全边界**——文件系统和网络完全不管，mode 在它上面是空话 |
| `os` | 系统自带的隔离原语，无守护进程、无镜像、一次 exec 的开销 | macOS 用 Seatbelt（`sandbox-exec`），Linux 用 Landlock。**默认走这条** |
| `docker` | 临时容器：无网络、只读 rootfs、内存/PID 限额、只挂工作目录 | 最强也最重；容器化部署里还得挂 docker socket，那等于交出宿主 root，所以要显式 `allow_docker: true` |

留空自动选：`allow_docker` 且有 docker → docker；否则能用 os 就用 os；都不行才退到 native。**降级永远会说出来**（`Result.Degraded`，同时进审计日志）——一次看起来被沙箱包住、实际没有的执行，比压根没有沙箱更糟。

## 各平台实际强制了什么

| 平台 / 内核 | 读 | 写 | 网络 |
|---|---|---|---|
| macOS Seatbelt | 白名单 | 工作目录 | 断得掉 |
| Linux Landlock ABI ≥ v4（内核 6.7+） | 白名单 | 工作目录 | 断 TCP |
| Linux Landlock v1–v3（含 Debian 12 的 6.1） | 白名单 | 工作目录 | **断不掉**，每次执行都会带降级说明 |
| Linux 无 Landlock | — | — | — → 退回 native |

### 已知缺口

- **UDP / QUIC**：Landlock 的网络规则只管 TCP 的 bind/connect，UDP 出网不在它管辖内。真要彻底断网，用 docker 后端。
- **Debian 12 是常见情况不是边角**：它的 6.1 内核没有 Landlock v4，文件系统限得住、出网限不住。Web 的「脚本沙箱设置」里会直接把本机的实际能力写出来。
- **读取放得比写宽**：解释器和标准库要能读，所以系统目录整体放开。别把 agent 的配置目录写进 `read_paths`。

## Linux 是怎么实现的

Go 没法在 fork 和 exec 之间插代码，所以 Landlock 没办法由父进程施加给子进程。做法跟 Codex CLI 一样——再执行一次自己：

```
jelly __sandbox --ro=/usr --ro=/bin … --rw=<技能目录> --no-net -- python3 x.py
```

这个隐藏子命令先把限制加在**自己**身上，再 `exec` 真正的目标，所以目标从第一条指令起就已经被关住，不存在一个「还没上锁」的窗口。单二进制，无需额外安装，无需特权。

父子之间的 argv 约定在 `internal/sandbox/helper.go`，拼装和解析都不带平台标签，因此在 macOS 上也能跑往返测试——Landlock 本身只能在 Linux 上验，但这个接缝不必。

## 按技能收紧

技能的 frontmatter 里可以写 `sandbox:`：

```markdown
---
name: check-pod-health
description: 巡检指定命名空间的 Pod 状态
enabled: true
sandbox: read-only
---
```

**只能往严了改，不能往松了改。** 技能是内容，全局策略是配置；让内容放宽自己的权限，全局策略就成了建议。技能声明 `workspace-net` 而全局是 `workspace`，最终仍然是 `workspace`。

`use_skill` 会把最终生效的档位告诉模型，省得它写个 curl 再撞墙重试。

## 怎么验

```bash
go test ./internal/sandbox/          # macOS 上会真跑 Seatbelt 的强制效果
```

Linux 侧的强制效果需要一台开了 Landlock 的内核；Docker Desktop 的 LinuxKit 内核没编 Landlock，在那里 os 后端会如实报告不可用并退回 native。

## 运行镜像

`run_script` 要在容器里真跑起来，镜像必须带解释器。`gcr.io/distroless/base-debian12` 里没有 sh 也没有 python3，`sandbox.Interpreters` 声明的四种脚本一种都起不来——所以 Dockerfile 的运行层是 `python:3.12-slim-bookworm`（镜像 86MB → 254MB）。生产不打算用 run_script 的话，关掉 `skills.allow_scripts` 并把那层换回 distroless，攻击面更小。
