# syntax=docker/dockerfile:1
# 构建阶段固定在宿主平台运行，按 TARGETARCH 交叉编译（多架构构建无需 qemu 模拟 Go 编译）
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party ./third_party
ARG GOPROXY=https://goproxy.cn,direct
RUN GOPROXY=${GOPROXY} go mod download
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/cline2api .

FROM alpine:3.22
# su-exec：entrypoint 修正 /data 属主后降权运行（避免 bind mount 属主不匹配导致启动失败）
RUN apk add --no-cache ca-certificates su-exec && addgroup -S -g 100 app && adduser -S -G app -u 100 app
WORKDIR /app
COPY --from=builder /out/cline2api /usr/local/bin/cline2api
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
# 防 Windows CRLF 混入：剔除 \r，否则 busybox sh 报 '\r' 未找到
RUN sed -i 's/\r$//' /usr/local/bin/entrypoint.sh && chmod +x /usr/local/bin/entrypoint.sh && mkdir -p /data && chown app:app /data
# 以 root 启动 entrypoint（chown /data 需 root），进程随后降权为 app（100:100）
ENV PORT=8080 DATA_DIR=/data PUID=100 PGID=100
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
