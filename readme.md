# S3 Client

一个 Windows 桌面端 AWS S3 客户端，支持 S3 兼容存储（如 MinIO）。使用 Go + Fyne 构建，提供 GUI 和 CLI 两种入口。

## 功能

- **S3 账号管理**：新增、编辑、删除账号，支持自定义 Endpoint、Region、Path-Style
- **密码安全存储**：Secret Access Key 经 AES-256-GCM 加密后存入 SQLite（`~/.s3client/data.db`），密钥由主密码经 scrypt 派生，**绝不明文存储**
- **主密码保护**：首次使用设置主密码，后续启动需输入主密码解锁
- **Bucket 浏览**：双击账号查看其下所有 Bucket
- **对象浏览器**：双击 Bucket 列出文件和文件夹，文件夹可继续下钻
- **可点击面包屑路径**：路径每一段可点击，直接跳转到对应目录
- **模糊过滤**：输入关键字实时筛选当前目录下的文件和文件夹
- **文件元数据**：文件列表显示大小、类型、最后修改时间，并带列头
- **删除二次确认**：删除文件/文件夹需连续两次确认，防止误删
- **可调过期的预签名 URL**：自定义过期分钟数（默认 10 分钟），URL 随输入实时刷新
- **文件操作**：上传文件（带进度条）、下载文件（可选路径 + 进度条）、删除文件/文件夹、新建文件夹
- **系统原生文件对话框**：上传选文件、下载选保存路径均使用操作系统原生对话框（ncruces/zenity）
- **下载后打开文件夹**：下载完成自动在系统文件管理器中打开并选中文件
- **Bucket 权限检查**：双击账号后用 HeadBucket 检查每个 Bucket 的访问权限，固定列显示 可访问/无权限；可访问的桶名显示绿色可双击进入，无权限的显示灰色且禁止双击
- **右键查看属性**：Bucket/文件夹/文件均可右键查看属性，属性值可 Ctrl+C 复制
- **菜单栏**：解锁后提供「加锁」「修改主密码」「修改 SQLite 存储位置」（修改主密码会重新派生密钥并重加密所有账号，事务保证原子性）
- **错误消息区**：进入无权限 Bucket 时在底部可滚动消息区显示错误，并保留返回按钮
- **预签名 URL**：为文件生成预签名 GET URL，过期时间可自定义（默认 10 分钟）
- **测试连接**：一键验证账号凭证是否有效
- **CLI 入口**：纯 Go 命令行工具，无需 CGO，支持 list/add/del/test/buckets 命令

## 技术栈

| 组件 | 技术 |
|------|------|
| GUI | [Fyne v2](https://fyne.io/) |
| S3 | [AWS SDK for Go v2](https://github.com/aws/aws-sdk-go-v2) |
| 数据库 | [modernc.org/sqlite](https://modernc.org/sqlite)（纯 Go，无需 CGO） |
| 加密 | AES-256-GCM + [scrypt](https://pkg.go.dev/golang.org/x/crypto/scrypt) 密钥派生 |
| CLI 密码输入 | [golang.org/x/term](https://pkg.go.dev/golang.org/x/term) |

## 目录结构

```
main.go                   GUI 入口
cmd/cli/main.go           CLI 入口（纯 Go，无需 CGO）
internal/
  crypto/crypto.go        AES-256-GCM 加解密 + scrypt 密钥派生
  model/account.go        账号数据模型
  store/store.go          SQLite 存储层（主密码管理 + 账号 CRUD）
  awss3/client.go         S3 操作封装（列桶/列对象/上传/下载/删除/预签名/新建文件夹）
  ui/ui.go                Fyne GUI 界面
```

## 构建与运行

项目支持 **Windows / macOS / Linux** 三平台。推荐用 `make`（在各自系统上原生构建）：

```bash
make build       # 为当前系统构建 GUI（Windows 自动隐藏控制台并链接 exe 图标）
make build-cli   # 构建纯 Go CLI（跨平台，无需 CGO）
make run         # 直接运行 GUI
```

> GUI 依赖 **CGO**（Fyne 需要各平台的 C 工具链和图形库），需在**目标系统上原生构建**；CLI 是纯 Go，任意平台可交叉编译。

### 各平台构建依赖

| 平台 | 构建依赖 | 运行依赖 |
|------|----------|----------|
| **Windows** | MinGW-w64 的 `gcc`（如 MSYS2 ucrt64）；构建前 `export PATH=/c/msys64/ucrt64/bin/:$PATH` | 无 |
| **macOS** | Xcode Command Line Tools（`xcode-select --install`） | 无 |
| **Linux** | `gcc` + 图形开发库：`libgl1-mesa-dev`、`xorg-dev`（Debian/Ubuntu）或对应包 | **`zenity`**（文件对话框需要，`apt/dnf install zenity`） |

### 手动构建（不使用 make）

```bash
# GUI —— Windows
export PATH=/c/msys64/ucrt64/bin/:$PATH
goversioninfo -64 -o resource_windows.syso   # 生成 exe 图标（仅 Windows；必须 -64）
CGO_ENABLED=1 go build -ldflags "-H windowsgui" -o s3client.exe .

# GUI —— macOS / Linux（不要加 -H windowsgui）
CGO_ENABLED=1 go build -o s3client .

# CLI —— 任意平台
CGO_ENABLED=0 go build -o s3cli ./cmd/cli
```

CLI 命令：`list` | `add` | `del <id>` | `test <id>` | `buckets <id>` | `help` | `exit`

### 跨平台打包与图标

- Windows 的 exe 图标由 `resource_windows.syso`（`goversioninfo -64` 生成，含 GOOS 后缀，仅 Windows 参与链接）提供。
- 各平台带图标的应用打包推荐 [`fyne`](https://docs.fyne.io/) CLI：准备一张 `Icon.png` 后 `make package-mac` / `package-linux` / `package-windows`（配置见 `fyneapp.toml`）。
- 从**单台机器交叉构建**其它系统的 GUI 二进制，推荐 [`fyne-cross`](https://github.com/fyne-io/fyne-cross)（基于 Docker，自带各平台交叉工具链）。

> 说明：核心逻辑、CLI 及纯 Go 包已验证可交叉编译到 macOS / Linux；GUI（CGO）需在目标系统或用 fyne-cross 构建，尚未在 Mac/Linux 实机验证。

## 安全设计

- Secret Access Key **不以明文存储**
- 主密码经 scrypt（N=32768, r=8, p=1）派生 32 字节密钥
- 每次加密使用随机 12 字节 nonce + AES-256-GCM
- 主密码本身不落库，仅存加密校验器用于解锁验证
- 数据库文件位于 `~/.s3client/data.db`，权限 0700
