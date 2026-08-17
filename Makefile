APP     := s3client
CLI     := s3cli
PKG     := .
CLI_PKG := ./cmd/cli
GOOS    := $(shell go env GOOS)

.PHONY: build build-cli run icon tools clean \
        package-windows package-mac package-linux

## build: 为当前系统构建 GUI（Windows 自动隐藏控制台并生成 exe 图标）
build:
ifeq ($(GOOS),windows)
	$(MAKE) icon
	CGO_ENABLED=1 go build -ldflags "-H windowsgui" -o $(APP).exe $(PKG)
	@echo "Built $(APP).exe"
else
	CGO_ENABLED=1 go build -o $(APP) $(PKG)
	@echo "Built $(APP)"
endif

## build-cli: 构建纯 Go CLI（跨平台，无需 CGO）
build-cli:
	CGO_ENABLED=0 go build -o $(CLI) $(CLI_PKG)
	@echo "Built $(CLI)"

## run: 直接运行 GUI
run:
	CGO_ENABLED=1 go run $(PKG)

## icon: 生成 Windows exe 图标资源（需 goversioninfo；必须用 -64 生成 64 位）
icon:
	goversioninfo -64 -o resource_windows.syso

## tools: 安装本项目用到的辅助工具
tools:
	go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest
	go install fyne.io/tools/cmd/fyne@latest

## package-windows: 用 fyne 打包 Windows 应用（需 fyne CLI + Icon.png）
package-windows:
	fyne package -os windows -icon Icon.png

## package-mac: 用 fyne 打包 macOS .app（需 fyne CLI + Icon.png）
package-mac:
	fyne package -os darwin -icon Icon.png

## package-linux: 用 fyne 打包 Linux 应用（需 fyne CLI + Icon.png）
package-linux:
	fyne package -os linux -icon Icon.png

## clean: 清理构建产物
clean:
	rm -f $(APP) $(APP).exe $(CLI) $(CLI).exe
