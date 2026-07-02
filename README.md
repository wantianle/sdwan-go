# sdwan-go

Go 实现的 SD-WAN VPN 隧道客户端，跨平台支持。

| 平台 | 控制方式 |
|------|---------|
| Windows | 托盘面板（Wails v2 WebView2） |
| Linux | systemd 服务 + CLI 子命令 |
| macOS | LaunchDaemon 服务 + CLI 子命令 |

## 架构

```
panel (Wails) / CLI  ──HTTP──▶  sdwan daemon  ──UDP──▶  SD-WAN server
                                    │
                                    ├─ TUN/Wintun/utun
                                    ├─ IP + 路由
                                    └─ 控制 API (:17890)
```

核心进程以 **daemon 模式** 运行，拥有 TUN 网卡、IP 和路由的生命周期。面板和 CLI 通过 localhost HTTP 控制 API 与之通信，不直接操作网卡或路由。daemon 启动失败时不退出，后台自动重连（指数退避 500ms~8s），auth 拒绝时永久停止。

## 目录结构

```
sdwan-go/
├── cmd/sdwan/main.go              ← 入口（daemon / CLI）
├── internal/core/                 ← 隧道核心
│   ├── client.go                  ← UDP 会话管理、重连、暂停
│   ├── config.go                  ← iwan.conf 解析
│   ├── control.go                 ← HTTP 控制 API 服务端
│   ├── protocol.go                ← SD-WAN 协议（OPEN, ECHOREQ, AES/MD5）
│   ├── runner.go                  ← daemon / one-shot 启动编排
│   ├── route_conflict.go          ← 路由重叠检测
│   ├── protocol_test.go
│   └── tunnel_*.go                ← 平台 TUN 实现
├── pkg/                           ← 共享包（core & panel 共用）
│   ├── protocol/                  ← 协议常量、构建解析、加密
│   └── controlapi/                ← 控制 API 类型与 HTTP 客户端
├── panel/                         ← Windows 托盘面板（独立 Go module）
│   ├── main.go / app.go / systray.go
│   ├── core/manager.go / control.go
│   └── frontend/
├── scripts/
│   ├── install.sh / uninstall.sh
│   └── install.ps1 / uninstall.ps1
├── iwan.conf                      ← 配置模板（仅参考）
├── Makefile / go.mod / go.sum
└── README.md
```

## 一键安装

**Linux / macOS：**
```bash
curl -fsSL https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/install.sh | sudo bash
```

**Windows（管理员 PowerShell）：**
```powershell
iwr -useb https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/install.ps1 | iex
```

> 直连 GitHub 失败时脚本会自动切换镜像（gh.ddlc.top → gh-proxy.com → gh.idayer.com）。

**安装指定版本：**
```bash
curl -fsSL .../install.sh | sudo bash -s -- 1.2.3
```

```powershell
iwr -useb .../install.ps1 | iex
# 然后输入版本号提示时填 1.2.3，或用安装目录里的 install.ps1 -Version 1.2.3
```

## 配置文件 (iwan.conf)

| 路径 | 平台 |
|------|------|
| `/etc/sdwan/iwan.conf` | Linux / macOS |
| `C:\ProgramData\sdwan\iwan.conf` | Windows |

```ini
server=minieye.9966.org
username=your_username
password=your_password
port=10010
mtu=1436
encrypt=0
tunname=iwan1
routenet=192.168.0.0/16
```

| 字段 | 必填 | 说明 |
|------|:--:|------|
| `server` | ✅ | SDWAN 服务器地址 |
| `username` | ✅ | 工号 |
| `password` | ✅ | 密码 |
| `port` | | UDP 端口，默认 10010 |
| `mtu` | | 最大传输单元，默认 1436 |
| `encrypt` | | 0=明文，1=AES 加密 |
| `tunname` | | TUN 网卡名，默认 iwan1 |
| `routenet` | | 内网路由网段，CIDR 格式 |

## CLI 子命令

`sdwan -f iwan.conf` 启动 daemon 后，可用子命令控制：

```bash
# 查看当前状态（服务器、路由、session、冲突等）
sdwan status -f /etc/sdwan/iwan.conf

# 切换服务器
sdwan switch minieye.8866.org -f /etc/sdwan/iwan.conf
```

子命令通过读取同目录下的 `control.token` 与 daemon 的 HTTP API 通信。

## 控制 API (HTTP)

daemon 在 `127.0.0.1:17890` 提供 REST API，Bearer token 鉴权（token 文件与 daemon 同目录的 `control.token`）。

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/v1/status` | 状态：running/reconnecting/paused/disconnected，路由冲突 |
| `POST` | `/v1/switch` | `{"server":"host"}` 切换服务器 |
| `POST` | `/v1/pause` | `{"pause":true/false}` 暂停/恢复 |
| `POST` | `/v1/shutdown` | 优雅退出 |

## 多平台兼容性

| 平台 | TUN 驱动 | 权限 |
|------|---------|------|
| Linux | `/dev/net/tun`（内核自带） | root |
| macOS | `utun`（内核自带） | root |
| Windows | `wintun.dll` | 管理员 |

## 验证隧道

```bash
ping 10.10.10.1
ping hfs.minieye.tech    # 应解析到 192.168.x.x
```

## 卸载

**Linux / macOS：**
```bash
curl -fsSL https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/uninstall.sh | sudo bash
```

**Windows（管理员 PowerShell）：**
```powershell
iwr -useb https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/uninstall.ps1 | iex
```

## 开发

```bash
make          # 构建、vet、test
make test     # go test -race
make vet      # go vet
make tidy     # go mod tidy
```

跨平台构建：
```bash
GOOS=windows GOARCH=amd64 go build ./cmd/sdwan
GOOS=linux   GOARCH=amd64 go build ./cmd/sdwan
GOOS=darwin  GOARCH=amd64 go build ./cmd/sdwan
```
