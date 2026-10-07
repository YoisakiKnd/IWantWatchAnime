# 交叉编译镜像：在 x86 上构建出 armv7 镜像，不需要在玩客云上装 Go。
#
#   docker buildx build --platform linux/arm/v7 -f deploy/Dockerfile -t suzu:armv7 --load .
#
# 注意：这个镜像里只有 suzu 本体，不含 aria2。
# 推荐用 compose 起两个服务（suzu + aria2），或让宿主机的 aria2 常驻，
# 两边通过 127.0.0.1:6800 的 JSON-RPC 通信。

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
WORKDIR /src

# 先只拷依赖清单，让 go mod download 这一层能被缓存住
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=arm
ARG TARGETVARIANT=7
ARG VERSION=dev
RUN set -eux; \
    GOARM=""; \
    if [ "$TARGETARCH" = "arm" ]; then GOARM="GOARM=${TARGETVARIANT:-7}"; fi; \
    env CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" $GOARM \
      go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/suzu ./cmd/suzu; \
    ls -lh /out/suzu

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 1000 suzu && \
    mkdir -p /data /downloads /library && \
    chown -R suzu:suzu /data /downloads /library

COPY --from=build /out/suzu /usr/local/bin/suzu

USER suzu
WORKDIR /data
VOLUME ["/data", "/downloads", "/library"]
EXPOSE 8637

# 邮件通知类功能需要正确的时区；默认给上海，可被 TZ 覆盖
ENV TZ=Asia/Shanghai

ENTRYPOINT ["/usr/local/bin/suzu"]
CMD ["-config", "/data/config.toml"]