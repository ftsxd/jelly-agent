# 工具元数据放哪

三层，从下到上：

| 层 | 位置 | 谁维护 | 什么时候用 |
|---|---|---|---|
| 随产品发布 | `internal/toolreg/bundled/*.yaml` | 仓库，编译进二进制 | 这个构建认识的工具（如 n9e） |
| 部署自己的 | `tools.metadata_dir` 指向的目录 | 运维，文件 | 你这套部署特有的 MCP 工具 |
| 控制台改的 | `tool_decls` 表 | 界面，带审计 | 现场调整，逐字段覆盖上面两层 |

`n9e.yaml` 原来在这个目录里，靠 `tools.metadata_dir` 指过来才生效——而没人
设过那个键，容器镜像里也根本没有 `configs/`，所以它从来没有生效过。现在它
编译进二进制（`internal/toolreg/bundled/`），不需要任何启用步骤。

要给你自己的 MCP 服务器加声明，在 `tools.metadata_dir` 指向的目录里放
`*.yaml`，格式和 bundled 里的一样。默认目录是配置文件旁边的 `tools/`，
也就是 `~/.jelly-agent/tools/`。
