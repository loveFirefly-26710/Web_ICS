# 语法参考：https://docs.docker.com/reference/dockerfile/
# 工具链版本必须和 go.mod 的 go 指令、以及 build.sh 的要求保持一致。
# 用 1.23 构建会带上 34 条已公开的标准库漏洞，其中 4 条落在 HTTP 请求
# 处理路径上，所以这里和 build.sh 一样要求 1.25。
FROM golang:1.25-alpine AS build
WORKDIR /src

# 先拷依赖清单，利用构建缓存
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
# CGO_ENABLED=0 保证纯静态链接，运行阶段无需任何 libc。
# -trimpath 去掉构建机上的绝对路径：不加的话二进制里会留下源码目录与
# go/pkg/mod 模块缓存目录这类绝对路径，交付出去等于泄露构建环境。
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/web_ics ./cmd/web_ics

# ---------------------------------------------------------------- 运行阶段
FROM alpine:3.20
RUN apk add --no-cache tzdata ca-certificates

# 非 root 运行。语料目录要在这里建好并改属主，具名卷会继承镜像里
# 该目录的属主，不先建的话挂上来的卷归 root，应用写不进去。
RUN adduser -D -u 1000 web_ics \
 && mkdir -p /var/lib/web_ics-corpus /app \
 && chown -R web_ics:web_ics /var/lib/web_ics-corpus /app

ENV TZ=Asia/Shanghai

WORKDIR /app
# 前端资源已经编译进二进制了（见 web 包），这里不用再拷 web/ 目录，
# 也就不需要 WEB_ICS_WEB_ROOT。要临时换前端就挂一个目录并设它覆盖。
COPY --from=build /out/web_ics /app/web_ics

# 文档库只读挂载点；语料库需要可写（建语料时要在里面落分片）
VOLUME ["/docs", "/var/lib/web_ics-corpus"]

ENV WEB_ICS_DOC_ROOT=/docs \
    WEB_ICS_CORPUS_DIR=/var/lib/web_ics-corpus \
    WEB_ICS_ADDR=:8080 \
    WEB_ICS_MAX_CONCURRENCY=10 \
    GOMEMLIMIT=300MiB

USER 1000:1000

EXPOSE 8080

# 健康检查：/healthz 在内存降级时返回 ok:false，但 HTTP 仍是 200。
# 这里只探连通性，降级是正常的保护状态，不该让容器重启。
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/web_ics"]
CMD ["--doc-root", "/docs", "--corpus-dir", "/var/lib/web_ics-corpus", "--addr", ":8080"]
