FROM node:22-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.25-bookworm AS build
WORKDIR /src
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/jelly ./cmd/cli
RUN mkdir -p /out/data/.jelly-agent && touch /out/data/.jelly-agent/.keep

# 运行镜像带解释器，否则 run_script 在容器里根本跑不起来：distroless/base 里没有
# sh、没有 python3，sandbox.Interpreters 声明的四种脚本一种都执行不了。换成
# python:slim 是为技能脚本付的代价（镜像 86MB → 254MB，且多了一个 shell），
# 换来的是沙箱执行这条路在线上真正通。不打算在生产用 run_script 的话，把
# skills.allow_scripts 关掉，再把这层换回 gcr.io/distroless/base-debian12:nonroot。
# 镜像本身不是隔离边界——边界由 internal/sandbox 的 os 后端（Landlock）提供。
FROM python:3.12-slim-bookworm
# git 不是可选的：代码同步技能靠它拉代码，而 python:slim 里既没有 git 也没有 curl
# （实测确认）。少了它，周期同步任务在容器里会以「command not found」失败，而那
# 看起来完全不像是镜像的问题。
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN useradd --uid 65532 --user-group --home-dir /data --shell /usr/sbin/nologin nonroot
COPY --from=build /out/jelly /usr/local/bin/jelly
COPY --chown=nonroot:nonroot --from=build /out/data /data
USER nonroot:nonroot
ENV HOME=/data
EXPOSE 6185
ENTRYPOINT ["/usr/local/bin/jelly"]
CMD ["serve", "--addr", "0.0.0.0:6185"]
