# AGENTS.md

哪吒监控 Agent 的精简 fork，运行在被监控主机上：采集主机指标，经 gRPC 上报给 [wangdefaa/nezha](https://github.com/wangdefaa/nezha)，执行拨测，并自更新。相比上游已删除命令执行、Web 终端、文件管理、NAT、配置下发、服务器转移、MCP 和 GPU/温度采集。

## 命令

```bash
go run ./cmd/agent -c config.yml                  # -c 留空时读二进制同目录的 config.yml
nezha-agent edit -c config.yml                    # 交互式编辑配置（网卡、磁盘、DNS、UUID）
nezha-agent service -c config.yml install         # 也支持 uninstall/start/stop/restart；自动选择 systemd/OpenRC/launchd
go vet ./... && gofmt -l . && go test -race -shuffle=on -count=1 ./...   # 与 CI 一致
CGO_ENABLED=0 go build -ldflags="-s -w" -o nezha-agent ./cmd/agent
```

## 发版

- 推 `v*` tag → 先过 test → GoReleaser 构建 7 个目标：linux amd64/arm64/arm(v6)、darwin amd64/arm64、windows amd64/arm64 → 打包成 `nezha-agent_{os}_{arch}.zip`。
- 用 secret `AGENT_SIGNING_KEY` 签出同名 `.sig`，再用 `updater.go` 内置的公钥验签，不配对就不发版。发布用 `gh release create`，同名 release 已存在就失败，不覆盖资产。
- 资产名是自更新和安装脚本的契约，改名会让已部署的 agent 无法升级。版本号经 ldflags 注入 `pkg/monitor.Version`，必须是不带 v 的 semver。
- `install.sh` 用内置公钥和 openssl 验签；没有 openssl 时要显式设置 `NZ_ALLOW_UNSIGNED=1`。`install.ps1` 按 `checksums.txt` 校验 sha256。两个脚本里的公钥要和 `updater.go` 一起轮换。
- `Dockerfile` 和 `docker-compose.yaml` 只用于本地构建，不发布镜像。

## 运行模型

- `run()` 循环建连，每次连接是一个 `agentSession`（`session.go`）。
  - 先调 `ReportSystemInfo2`，拿回面板启动时间。
  - 再起两个协程：`reportStateDaemon` 推送状态，`receiveTasksDaemon` 接收任务。任一出错，就结束会话并重连。
  - 跨重连保留的状态一律用原子量。阻塞的流操作用 `callWithDeadline` 限时：收任务 30 秒，推状态 10 秒。
- 每个任务各起一个协程执行，同时最多 256 个；超出时直接回复 "agent busy"。自更新同一时间只跑一个。
- 任务类型只剩 HTTP(1)、ICMP(2)、TCP(3)、Upgrade(6)、Keepalive(7)。删掉的编号保留不复用，以兼容协议。`DisableSendQuery=true` 时拒绝所有拨测。
- 协议：`proto/nezha.proto` 必须和面板一致，改动要两端同步并重新生成 `.pb.go`。鉴权在每次调用的 metadata 里带 `client-secret` / `client-uuid`；`tls:true` 时拒绝在明文信道上发送凭据。

## 坑

- 没有配置热重载，改配置要重启。`config.yml` 含密钥，已加入 gitignore；`report_delay` 限制在 1–4。
- `RequestTask` 流被多个任务协程共享，所有 `TaskResult` 都必须经 `newSerialTaskResultSender` 串行发送。
- Linux 下 `service install` 会创建低权限用户 `nezha`，并注入 `CAP_NET_RAW` 以支持 ICMP。程序目录只有是 agent 专用目录时才改属主（非递归、不跟随符号链接），否则自更新不可用。
- 配置文件在读和写时都会收紧到 0600，避免 `client_secret` 被同主机的其他用户读到。
- 自更新从 `wangdefaa/nezha-agent` 的 release 下载，校验同名 `.sig`（签名只覆盖 zip 内容）。多实例互斥用程序目录下的 `.<exe>.stat`，不要放到共享的 /tmp。`disable_auto_update` / `disable_force_update` 分别关闭定时自更新和面板下发的强制更新，容器或内网部署建议都设为 true。
- HTTPS 拨测用 uTLS 伪装 Chrome 指纹，拨号和握手各有 10 秒超时，不要在全局锁里做网络 I/O。响应体最多读 1 MiB，只允许 http/https。
- DNS 走 Go 内置 resolver，只用 `dns` 配置里的服务器（留空时用内置的公共 DNS，不读 /etc/resolv.conf）。ICMP/TCP 拨测只有配置了 `dns` 时才用它，否则用系统 DNS。

## 约定

- 注释和回答用中文；函数不超过 30 行；commit message 用中文，不超过 50 字。
- 上游同步一律手工移植，按代码实质判断某项修复是否已经移植；涉及已删除模块的改动直接跳过。
