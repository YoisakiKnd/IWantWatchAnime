APP      := suzu
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
GOFLAGS  := -trimpath   # 作为 go 子命令的参数使用，不要放进环境变量前缀
BIN      := bin

.PHONY: help build all armv7 armv6 arm64 amd64 run test bench vet fmt size clean docker-armv7

help:
	@echo "make build          编译当前平台到 $(BIN)/$(APP)"
	@echo "make armv7          交叉编译 玩客云/N1/树莓派2-3(32位) 用 linux/arm GOARM=7"
	@echo "make arm64          交叉编译 树莓派3+/4/电视盒子 用 linux/arm64"
	@echo "make all            三个平台全编"
	@echo "make test           跑单元测试与端到端测试"
	@echo "make bench          跑标题解析基准（评估弱 CPU 是否吃得消）"
	@echo "make size           看各平台产物体积"

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/$(APP) ./cmd/suzu

# 玩客云：Amlogic S805 / 四核 Cortex-A5 / armv7l。
# CGO_ENABLED=0 是关键——没有 CGO 就不需要交叉编译工具链，
# 在一台 x86 笔记本上一条命令就能产出目标机可直跑的静态二进制。
armv7:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
		go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/$(APP)-linux-armv7 ./cmd/suzu

# 少数老内核/软浮点环境（比如某些 OpenWrt 固件）需要 GOARM=5。
armv6:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=5 \
		go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/$(APP)-linux-armv6 ./cmd/suzu

arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/$(APP)-linux-arm64 ./cmd/suzu

amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/$(APP)-linux-amd64 ./cmd/suzu

all: amd64 armv7 arm64

run:
	go run ./cmd/suzu -config config.toml -log-level debug

test:
	go test ./...

bench:
	go test ./internal/matcher/ -run XXX -bench . -benchmem

vet:
	go vet ./...

fmt:
	gofmt -l -w ./cmd ./internal

size:
	@ls -lh $(BIN)/ 2>/dev/null || echo "先 make all"

docker-armv7:
	docker buildx build --platform linux/arm/v7 -f deploy/Dockerfile \
		--build-arg VERSION=$(VERSION) -t $(APP):armv7 --load .

clean:
	rm -rf $(BIN)