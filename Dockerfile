# work2api 容器镜像：纯 Go（免 CGO），前端 dist 已 go:embed 进二进制，构建无需 Node。
# 多阶段：builder 交叉编译静态单文件，runtime 用 alpine（带 ca-certificates 以便
# 对上游发起 HTTPS）。以非 root 运行，数据落在 /data 卷。
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 \
    GOOS="${TARGETOS:-linux}" \
    GOARCH="${TARGETARCH:-amd64}" \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/work2api ./cmd/server

FROM alpine:3.22

# ca-certificates：对 WorkBuddy/Qoder/OpenCode 上游发起 TLS 必需；tzdata：签到/清理
# 等定时任务按本地时区结算。
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S work2api \
    && adduser -S -G work2api -h /data work2api \
    && mkdir -p /data \
    && chown -R work2api:work2api /data

COPY --from=builder /out/work2api /usr/local/bin/work2api

# 容器内必须绑 0.0.0.0（否则宿主端口映射打不进来）。DATA_DIR 指向卷，DB/凭据/密钥
# 都落在这里，随卷持久化。
ENV HOST=0.0.0.0 \
    PORT=8787 \
    DATA_DIR=/data

USER work2api
WORKDIR /data
VOLUME ["/data"]

EXPOSE 8787

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8787/health || exit 1

ENTRYPOINT ["/usr/local/bin/work2api"]
