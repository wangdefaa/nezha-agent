# CLAUDE.md

哪吒监控 Agent 的**精简改造 fork**(被控端,Go)。运行在被监控主机上,周期性采集主机指标(CPU/内存/磁盘/网络/负载等),经 **gRPC** 上报给精简版面板 [wangdefaa/nezha](https://github.com/wangdefaa/nezha)。

> 相比上游 [nezhahq/agent](https://github.com/nezhahq/agent),本 fork **移除了命令执行、Web 终端、文件管理、NAT 穿透、配置下发、服务器转移、MCP、GPU/温度采集**,只保留「采集 + 上报 + 主动拨测 + 自更新」,收敛权限、缩小被滥用面。

## 常用命令

```bash
# 运行(前台);留空 -c 时默认读二进制同目录 config.yml
go run ./cmd/agent -c config.yml
nezha-agent -c config.yml

# 交互式编辑配置(选网卡 / 磁盘分区 / DNS / UUID / debug,写回 config 文件)
nezha-agent edit -c config.yml

# 注册 / 卸载系统服务(systemd / OpenRC / launchd,按 init 系统自动择一)
nezha-agent service -c config.yml install      # install/uninstall/start/stop/restart

# 测试(CI 在 ubuntu/windows/macos 三平台跑)
go test -v ./...

# 本地构建(纯 Go,CGO 关闭)
CGO_ENABLED=0 go build -ldflags="-s -w" -o nezha-agent ./cmd/agent
```

发布构建用 GoReleaser(`.goreleaser.yml`),跨平台矩阵:
- **GOOS**: linux / windows / freebsd / darwin
- **GOARCH**: amd64 / 386 / arm / arm64 / mips / mipsle / s390x / riscv64 / loong64(windows/arm 排除)
- 版本号通过 ldflags 注入 `pkg/monitor.Version` 与 `main.arch`。Docker 镜像见 `Dockerfile`(golang:alpine 多阶段 + ca-certificates/tzdata)。

## 技术栈与关键依赖

- **Go 1.26**;CLI 框架 `urfave/cli/v2`
- **gRPC** `google.golang.org/grpc` + `protobuf`(与面板通信的唯一通道)
- 指标采集 `shirou/gopsutil/v4`(CPU/内存/磁盘/进程/host info)
- 配置 `knadh/koanf/v2`(YAML 文件 + `NZ_` 环境变量),YAML 用 `sigs.k8s.io/yaml`
- 拨测 `prometheus-community/pro-bing`(ICMP);出网 HTTP 用 `refraction-networking/utls` 伪装 Chrome TLS 指纹(`pkg/utls`)
- 自更新 `nezhahq/go-github-selfupdate` + `inconshreveable/go-update`;服务管理 `nezhahq/service`

## 目录结构与架构要点

```
cmd/agent/main.go        # 入口:CLI 解析、gRPC 主循环、拨测/自更新任务处理(核心文件)
cmd/agent/updater.go     # 自更新源(GitHub release: wangdefaa/nezha-agent)
cmd/agent/commands/      # service(服务安装)、edit(交互式配置)
model/                   # config.go(AgentConfig)、auth.go(gRPC 鉴权)、task.go(任务类型常量)、host.go(数据结构 + PB() 转换)
pkg/monitor/             # 采集逻辑核心:monitor.go 汇总,myip.go GeoIP;子包 cpu/load/disk/nic/conn 分平台
pkg/util/ pkg/utls/      # DNS、HTTP、uTLS RoundTripper 等工具
proto/nezha.proto        # gRPC 协议契约(.pb.go 为生成产物,勿手改)
script/install.sh .ps1   # Linux/macOS 与 Windows 一键安装脚本
```

**数据流**:`preRun` 读配置并 `monitor.InitConfig` → `run()` 建立 gRPC 连接 → `ReportSystemInfo2` 上报硬件信息(换回面板启动时间)→ 起两个 daemon goroutine:
- `reportStateDaemon`:每 `report_delay` 秒 `TrackNetworkSpeed()` 后经双向流 `ReportSystemState` 推送 `State`;每 10 分钟重报 host 硬件,每 `ip_report_period` 秒重报 GeoIP。
- `receiveTasksDaemon`:从 `RequestTask` 双向流收 `Task`,**每个 task 各起一个 goroutine** 执行(拨测可能很久),结果回传。

**任务模型(已精简)**:`doTask` 仅支持 `model/task.go` 中保留的类型——HTTPGet(1)、ICMPPing(2)、TCPPing(3)、Upgrade(6,强制自更新)、Keepalive(7)。命令执行/终端/NAT/文件管理/配置下发等中间号段**已删除但保留原数值不复用**,以兼容后端协议。`DisableSendQuery=true` 时三种拨测一律拒绝。

## 与 dashboard 的协议契约

- **proto**:`proto/nezha.proto`,service `NezhaService`。当前 fork 实际使用 `ReportSystemInfo2`(返回面板启动时间 `Uint64Receipt`)、`ReportSystemState`(双向流)、`RequestTask`(双向流)、`ReportGeoIP`。改协议须**同步面板侧**并重新生成 `.pb.go`。
- **连接参数**(`config.yaml.example` 全字段有注释):
  - `server`:面板地址 `host:port`,gRPC 与面板**复用同端口**(默认 8008)。
  - `client_secret`(**必填**):需与面板 `agent_secret_key` 一致。
  - `uuid`:留空首次连接由面板分配并写回配置文件。
  - `tls` / `insecure_tls`:面板在反代后启 https 时 `tls:true`;自签证书配 `insecure_tls:true`。
- **鉴权**(`model/auth.go`):`AuthHandler` 经 gRPC PerRPCCredentials 在每次调用的 metadata 里带 `client-secret`/`client-uuid`(同时含下划线变体 `client_secret`/`client_uuid` 以兼容)。当 `tls:true` 时 `RequireTransportSecurity()` 返回 true,**拒绝明文信道泄露凭据**;`tls:false`(内网明文)照常工作。

## 重要约定与坑

- **配置热重载已移除**:`agentConfig` 由 `preRun` 一次性读入,运行期不再轮转(无并发写入,无 torn read)。改配置须重启 Agent。
- **gRPC 流不可并发 Send**:`RequestTask` 流被多个 task goroutine 共享,所有 `TaskResult` 必须经 `newSerialTaskResultSender` 的互斥串行化,否则会损坏流。
- **Linux 以低权限 `nezha` 用户运行**:`service install` 时(需 root)自动创建系统用户 `nezha`(GNU `useradd` 失败回退 busybox `adduser`),并 `chown` 程序目录给它以保留自更新写权限。systemd/OpenRC 模板**额外注入 `CAP_NET_RAW`**(`main.go` 中 `systemdScriptWithCapNetRaw`/`openRCScriptWithCapNetRaw`),使低权限用户仍能做 ICMP 拨测(raw socket)。非 Linux 平台不设此用户。
- **配置文件权限**:`Save()` 显式 `Chmod 0600`(`os.WriteFile` 仅在 CREATE 时应用 perm),避免 `client_secret` 落在 world-readable 文件里被同主机攻击者读取冒充。
- **自更新源**:指向 `wangdefaa/nezha-agent` 的 GitHub release(非上游)。`disable_auto_update`/`disable_force_update` 可分别关闭定时自更新与面板下发的强制更新;容器/内网部署建议设 true。Windows 下 `preRun` 会校验二进制 arch 与系统 arch 是否匹配。
- **采集平台差异**:swap 在 Windows/Darwin 走 `SwapMemory`,其余走 `VirtualMemory`;`conn` 子包按 `conn_linux.go` / `conn_fallback.go` 分平台;LXC 容器对 `MemUsed` 有特殊计算。`skip_connection_count`/`skip_procs_count` 可在高并发机器上省资源。
- **出网 HTTP 指纹**:HTTP 拨测与取公网 IP 走 uTLS 伪装的 Chrome 指纹 + 浏览器 UA;DNS 默认用 Go 内置 resolver,可经 `dns` 配置自定义。
- `config.yml`(实例配置含密钥)在 `.gitignore` 中;**勿提交**。`report_delay` 强制约束 1~4。
