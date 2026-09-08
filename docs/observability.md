# 接 OpenTelemetry

这套 agent 发三种信号，都走 OTLP：

| 信号 | 内容 | 由什么控制 |
|---|---|---|
| traces | 每次 agent 循环一个 span，工具调用、模型调用各一个子 span | `tracing.enabled` |
| metrics | 工具耗时、token 用量等 | `tracing.enabled` |
| logs | **提示词与模型回复原文**（`gen_ai.system.message` / `gen_ai.choice`） | `tracing.capture_content` |

第三行是最容易踩的一条：ADK 把内容走 OTel 的 **logs** 通道发，不是塞进 span
属性（那会让每条 trace 都胖一圈）。所以 `capture_content: true` 时，**后端必须
有 logs pipeline**，否则它拒收、导出器不断重试，表现为 stderr 上刷屏的导出错误。
Jaeger all-in-one 只收 traces，配它就会出现这个现象。

## 配置

`~/.jelly-agent/config.yaml`（容器里是 `/data/.jelly-agent/config.yaml`）：

```yaml
tracing:
    enabled: true
    endpoint: ${OTEL_ENDPOINT}   # host:port，不带协议前缀
    protocol: grpc               # grpc(4317) | http(4318)
    insecure: true               # 同一内网/同一 compose 网络内是对的
    capture_content: true        # 会把提示词和回复发出去，见下面的告诫
    sample_ratio: 1              # 省略即全采
```

`${OTEL_ENDPOINT}` 这种写法是支持的：配置在解析前会展开 `${...}`（只认这一种
形式，避免把 bcrypt 的 `$2a$...` 当成变量）。于是同一份配置文件在宿主机上指向
你现有的 collector、在 compose 里指向容器，不用改文件。变量没设时展开成空串，
此时回落到内置默认 `localhost:4317`。

**`capture_content` 的告诫**：它是排查 agent 最有用的一个开关——"它为什么选了
那个工具"一眼就能看出来——也是生产上最危险的一个。提示词里带着这次事故带进来
的一切，而可观测后端很少有数据库那种访问控制。按环境逐个决定，不要一把全开。

## 两种接法

### 一、已经有 collector（你现在就是这种）

配置里指到它即可，compose 不用加任何服务：

```bash
OTEL_ENDPOINT=172.16.5.128:4317 docker compose up -d
```

容器走默认 bridge 网络，能路由到宿主机所在网段的地址，所以 LAN 上的 collector
直接可达。要确认的只有两件事：那个 collector 有没有 logs pipeline（见上），以及
`insecure: true` 是否与它的监听方式一致（它若启用了 TLS 就得关掉这个）。

### 二、想在 compose 里自带一套

```bash
docker compose --profile obs up -d
```

起来之后：

- Grafana <http://localhost:3000>（默认 admin/admin，数据源已预置）
- agent 通过 `OTEL_ENDPOINT` 默认值 `lgtm:4317` 发过去——**服务名，不是
  localhost**：在 compose 网络里 localhost 指的是容器自己。

`grafana/otel-lgtm` 一个容器里装了 Tempo(traces) + Prometheus(metrics) +
Loki(logs) + Grafana，三种信号都收，正好对上这套 agent 发的东西。要拆成
collector + 各自后端也可以，只是多三个服务，收益是能在 collector 里做采样、
脱敏、多路分发——真要把提示词发到远端时，脱敏那一步就得放在那儿。

## 验证

```bash
docker compose logs -f jelly-agent | grep -i otel   # 有导出错误会在这里刷
```

没有报错、Grafana 里 Tempo 能按 `service.name=jelly-agent` 查到 span，就通了。
