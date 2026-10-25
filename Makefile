# frpeasy build tooling
# 目标 Alpine 部署：纯静态编译（CGO_ENABLED=0），不依赖系统库。

BINARY_PANEL := frpeasy-panel
BINARY_AGENT  := frpeasy-agent
BIN_DIR       := bin

# 本机平台，用于把"本机 agent"按规范文件名放进 bin/。
# 面板是按 frpeasy-agent-<os>-<arch>[.exe] 这个名字找文件的，
# 少了这一步，make build 出来的面板在「客户端安装」页会 404 ——
# 而产物其实就在仓库里，只是名字对不上，这种问题极难自查。
GOOS_LOCAL        := $(shell go env GOOS)
GOARCH_LOCAL      := $(shell go env GOARCH)
EXE_SUFFIX        := $(if $(filter windows,$(GOOS_LOCAL)),.exe,)
LOCAL_AGENT_ASSET := $(BIN_DIR)/$(BINARY_AGENT)-$(GOOS_LOCAL)-$(GOARCH_LOCAL)$(EXE_SUFFIX)
DATA_DIR      ?= ./data
VERSION       ?= 0.5.0

# 发布相关的仓库信息。改仓库就要改这里 —— 它同时决定：
#   - 绿色包 systemd unit 里写死的更新源（FRPEASY_UPDATE_BASE）
#   - 一键部署脚本 deploy.sh 里嵌的默认下载地址
REPO_SLUG     ?= 1370339317/frpeasy
UPDATE_BASE   ?= https://github.com/$(REPO_SLUG)/releases/latest/download

# 构建时间注入二进制：面板侧栏会显示它。
# go:embed 是编译期把 UI 打进二进制，改了前端却没重新 build 时页面会一直是旧的，
# 没有构建时间根本无法分辨"功能没生效"和"跑的是旧二进制"。
BUILD_TIME    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME)

# -trimpath 剥掉构建目录的绝对路径。两个作用：
#  1. 不把 C:\Users\<你>\... 这种本机路径泄漏进发布的二进制；
#  2. 同一份源码在不同目录构建出**字节相同**的产物，别人才能用
#     `sha256sum` 核对发布包和自己重建的结果。少了它，manifest 里的
#     哈希只对「我这台机器这一次构建」成立。
BUILD_FLAGS := -trimpath

# 文档自检脚本用 Python 跑（不引入额外依赖，Linux/macOS/Windows 都有）。
PYTHON ?= python3

.PHONY: all build stage-local web web-install web-dev dev-ui build-alpine build-alpine-amd64 build-alpine-arm64 build-arm build-386 build-windows build-android stage-agent manifest check-manifest check-docs bundle dist run tidy clean

all: build

# 本地开发：编译当前平台（便于在 PC 上调试）
#
# 注意 build 依赖 web：前端是独立子项目，产物要先由 Vite 构建出来，
# go:embed 才能把它打进二进制。没装 node 的环境可以用 build-go 跳过前端。
build: web build-panel build-agent stage-local

# 前端子项目（web/）：React 19 + TypeScript + Tailwind v4 + Vite
# 产物输出到 internal/webui/dist，由 internal/webui/embed.go 嵌入
web:
	cd web && npm run build

# 首次克隆后装依赖（有 node_modules 就跳过）
web-install:
	@test -d web/node_modules || (cd web && npm ci)

# 前端开发服务器：HMR + /api 代理到本机 8080 的 panel
# 用法：先 make build-go && ./frpeasy-panel --data ./data，再另开一个终端 make web-dev
web-dev:
	cd web && npm run dev

# 不重新构建前端，只编 Go（改后端时用，快）
build-go: build-panel build-agent stage-local

build-panel:
	go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY_PANEL) ./cmd/panel

build-agent:
	go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY_AGENT) ./cmd/agent

# 把本机平台的 agent 按面板要求的文件名放进 bin/（面板同级 bin/ 会被自动查找）。
#
# 注意：本机的 windows/amd64 agent 与 bin/ 里交叉编译的**同名**，这一步会覆盖它。
# make build 顺手改了 bin/ 却不动 manifest.json —— 这正是清单哈希过期最常见的来源。
# 发布前务必 make manifest 再 make check-manifest。
stage-local: build-agent
	@mkdir -p $(BIN_DIR)
	@cp $(BINARY_AGENT) $(LOCAL_AGENT_ASSET)
	@echo "本机 agent 已就位: $(LOCAL_AGENT_ASSET)"

# Alpine 部署产物（linux 静态，musl 兼容）：amd64 + arm64 + windows amd64（客户端）
build-alpine: build-alpine-amd64 build-alpine-arm64 build-arm build-386 build-windows

build-alpine-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_PANEL)-linux-amd64 ./cmd/panel
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_AGENT)-linux-amd64 ./cmd/agent

build-alpine-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_PANEL)-linux-arm64 ./cmd/panel
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_AGENT)-linux-arm64 ./cmd/agent

# 32 位 arm（armv7 / armeabi-v7a）与 386：老 Android 设备、树莓派 2、x86 工控机。
# 面板端这两个产物只用于客户端下发，部署面板仍用 amd64/arm64。
build-arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_AGENT)-linux-arm ./cmd/agent

build-386:
	CGO_ENABLED=0 GOOS=linux GOARCH=386 go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_AGENT)-linux-386 ./cmd/agent

# Windows 产物。面板也发 Windows 版：
# 很多人是「下载 exe 双击就跑」，不给 panel 产物，他就只能看到客户端而装不上服务端。
# 自更新在 Windows 同样可用 —— swapBinary 用的是 rename，
# Windows 允许重命名正在运行的 exe（但不允许覆盖）。
build-windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_AGENT)-windows-amd64.exe ./cmd/agent
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_AGENT)-windows-arm64.exe ./cmd/agent
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_PANEL)-windows-amd64.exe ./cmd/panel
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_PANEL)-windows-arm64.exe ./cmd/panel

# Android 客户端：Go 没有 GOOS=android 的可执行构建，android 的 arm64-v8a /
# armeabi-v7a 直接跑 linux 静态二进制（frp 官方对 Android 也是这么发的）。
# 面板侧 /api/agent/bin/android/{arch} 会把请求映射到下面这两个产物。
build-android: build-alpine-arm64 build-arm
	@echo "Android 客户端就绪: bin/$(BINARY_AGENT)-linux-arm64 (arm64-v8a), bin/$(BINARY_AGENT)-linux-arm (armeabi-v7a)"

# 把各平台 agent 二进制放进面板数据目录，供 /api/agent/bin/{os}/{arch} 下发
stage-agent: build-alpine
	@mkdir -p $(DATA_DIR)/bin
	cp $(BIN_DIR)/$(BINARY_AGENT)-linux-amd64        $(DATA_DIR)/bin/
	cp $(BIN_DIR)/$(BINARY_AGENT)-linux-arm64        $(DATA_DIR)/bin/
	cp $(BIN_DIR)/$(BINARY_AGENT)-linux-arm          $(DATA_DIR)/bin/
	cp $(BIN_DIR)/$(BINARY_AGENT)-linux-386          $(DATA_DIR)/bin/
	cp $(BIN_DIR)/$(BINARY_AGENT)-windows-amd64.exe  $(DATA_DIR)/bin/
	cp $(BIN_DIR)/$(BINARY_AGENT)-windows-arm64.exe  $(DATA_DIR)/bin/
	@echo "agent 二进制已就位: $(DATA_DIR)/bin/"

run: build-panel
	./$(BINARY_PANEL) --data ./data --listen :8080

# 调试前端产物用这个：直接从磁盘读 internal/webui/dist，改完刷新浏览器即可，
# 不用每次 go build（go:embed 是编译期嵌入的）。
#
# 日常改前端更好的做法是 make web-dev（Vite HMR + 接口代理），
# 只有要验证「打包进二进制的最终形态」时才用 dev-ui。
dev-ui: build-go web
	./$(BINARY_PANEL) --data ./data --listen :8080 --ui-dir ./internal/webui/dist

# 生成自更新清单（面板自身与客户端升级的唯一入口）。
#
# 用法：
#   make build-alpine                 # 交叉编译到 bin/
#   make manifest VERSION=0.4.0 NOTES="修复 xx"
# 然后把 bin/ 整目录（含 manifest.json）上传到静态目录 / GitHub Release，
# 面板「设置 → 版本与更新」里的更新源填那个目录即可。
#
# 客户端**不走**这个源：它只从面板自己下载（/api/agent/bin/…），
# 所以内网无外网时客户端升级照样可用。
manifest:
	go run ./cmd/genmanifest -dir $(BIN_DIR) -version $(VERSION) -notes "$(NOTES)"

# 发布前守卫：清单里的 sha256/size 必须与 bin/ 里的文件严格一致。
#
# 为什么需要它：清单生成本身不报错，错的是「生成之后又动过文件」。
# 只要在 make dist 之后单独重建过任何一个平台（很常见：临时改一行代码
# 验证某个平台），清单里的哈希就过期了。客户端下载后会校验 sha256，
# 不匹配就抛错不装 —— 表现是「升级一直失败」，而面板侧看起来完全正常
# （清单版本比客户端新，于是不停下发）。这种事故在发布现场极难定位。
#
# 用 `nothing` 代替默认的"先重算再校验"：重算完必然一致，那样校验就是假绿。
check-manifest:
	go run ./cmd/genmanifest -dir $(BIN_DIR) -verify

# 文档链接与锚点自检。改完 README/QUICKSTART 后跑一下：
# 中文标题锚点、跨文件相对路径、章节重排之后最容易悄悄失效，
# 而这些断链不会有任何报错，只是读者点过去 404。
check-docs:
	@$(PYTHON) tools/check_docs_links.py

# 绿色包：每个 linux 面板产物打一个 tar.gz（二进制 + systemd unit + 使用说明）。
# 人手动下载时用这个；裸二进制留给一键脚本和自更新。
bundle:
	bash tools/make_bundle.sh $(BIN_DIR) $(VERSION) $(BIN_DIR)/bundles "$(UPDATE_BASE)"

# 一条命令做出可发布的目录：全部平台产物 + 清单 + 绿色包
dist: build-alpine manifest bundle
	@echo ""
	@$(MAKE) --no-print-directory check-manifest
	@echo "发布目录 $(BIN_DIR)/ 已就绪（含 manifest.json，已校验）。"
	@echo "绿色包在 $(BIN_DIR)/bundles/（人下载用）；裸二进制给脚本和自更新用。"
	@echo "上传后把更新源填成：https://<你的静态地址>/  （目录即可，不必写文件名）"
	@echo ""
	@echo "注意：上传前若又重建过任何产物，务必重跑 make manifest && make check-manifest。"

tidy:
	go mod tidy

clean:
	rm -f $(BINARY_PANEL) $(BINARY_AGENT)
	rm -rf $(BIN_DIR)
