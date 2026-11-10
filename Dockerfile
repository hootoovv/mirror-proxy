# syntax=docker/dockerfile:1

# ---------- 构建阶段 ----------
FROM golang:1.23-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/mirror-proxy .

# ---------- 运行阶段 ----------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/mirror-proxy /usr/local/bin/mirror-proxy
COPY config.yaml /etc/mirror-proxy/config.yaml

ENV MIRROR_PROXY_CONFIG=/etc/mirror-proxy/config.yaml \
    MIRROR_PROXY_CACHE_DIR=/data/.cache

EXPOSE 8080

# 缓存目录挂载点：docker run -v mirror-cache:/data/.cache
VOLUME ["/data/.cache"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/mirror-proxy"]
CMD ["-config", "/etc/mirror-proxy/config.yaml"]
