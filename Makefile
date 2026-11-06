BINARY := coinoption
PKG := github.com/tp/coinoption/cmd/coinoption
BIN_DIR := bin
GO ?= go

# 交叉编译产物架构：默认同时出 amd64（x86_64 服务器）与 arm64（ARM 云主机 / 64 位树莓派）。
# 只编一个架构：make build-linux LINUX_ARCHS=amd64
LINUX_ARCHS ?= amd64 arm64
LINUX_TARGETS := $(addprefix build-linux-,$(LINUX_ARCHS))

.PHONY: build test vet fmt smoke build-linux FORCE clean

build: | $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/$(BINARY) $(PKG)

# Linux 交叉编译：CGO_ENABLED=0 → 纯静态二进制，不依赖目标机 glibc；
# -trimpath 去掉本机路径，-s -w 去符号表（体积更小，便于 scp 到服务器）。
build-linux: $(LINUX_TARGETS)

build-linux-%: FORCE | $(BIN_DIR)
	GOOS=linux GOARCH=$* CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" \
		-o $(BIN_DIR)/$(BINARY)-linux-$* $(PKG)

$(BIN_DIR):
	mkdir -p $(BIN_DIR)

# 伪目标：保证每次 make 都重新交叉编译（build-linux-<arch> 不能被 .PHONY 标记，
# 否则 make 会跳过模式规则查找，导致 "Nothing to be done"）。
FORCE:

test:
	$(GO) test ./... -race

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

smoke:
	$(GO) run ./cmd/coinoption --smoke --uly BTC-USDT

clean:
	rm -f $(BIN_DIR)/$(BINARY) $(BIN_DIR)/$(BINARY)-*
