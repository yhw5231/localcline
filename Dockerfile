# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party ./third_party
ARG GOPROXY=https://goproxy.cn,direct
RUN GOPROXY=${GOPROXY} go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cline2api .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=builder /out/cline2api /usr/local/bin/cline2api
RUN mkdir -p /data && chown app:app /data
USER app
ENV PORT=8080 DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/cline2api"]
