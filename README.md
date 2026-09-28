# 哪吒监控 Agent(精简版)

[![License: Apache 2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

基于 [哪吒监控 Agent](https://github.com/nezhahq/agent) 改造的被控端。移除命令执行、Web 终端、文件管理、NAT 穿透、GPU/温度采集等远程管控能力,只保留**采集、上报与主动拨测**,收敛权限、缩小被滥用面。经 gRPC 上报给精简版面板 [wangdefaa/nezha](https://github.com/wangdefaa/nezha)。

## 采集内容

CPU / 内存 / 磁盘 / 负载、实时与累计流量、TCP/UDP 连接数、进程数、公网 IP / GeoIP;主动拨测支持 TCP、ICMP(Ping)、HTTP。

## 安装

Linux / macOS:

```bash
env NZ_SERVER=面板IP:8008 NZ_CLIENT_SECRET=你的密钥 \
  sh -c "$(curl -fsSL https://raw.githubusercontent.com/wangdefaa/nezha-agent/main/script/install.sh)"
```

Windows(管理员 PowerShell):

```powershell
$env:NZ_SERVER="面板IP:8008"; $env:NZ_CLIENT_SECRET="你的密钥"
iwr -useb https://raw.githubusercontent.com/wangdefaa/nezha-agent/main/script/install.ps1 | iex
```

`NZ_CLIENT_SECRET` 需与面板的 `agent_secret_key` 一致;面板经 TLS 反代暴露时加 `NZ_TLS=true`;直连面板的明文端口时,密钥会以明文传输,只适合内网。安装脚本会校验发布包:Linux / macOS 用内置公钥验签(需要 `openssl`),Windows 核对 `checksums.txt`。Docker 部署见 [docker-compose.yaml](docker-compose.yaml)(采集宿主机指标需开启 host 网络与 PID 命名空间)。

预编译平台:Linux amd64 / arm64 / armv6+、macOS amd64 / arm64、Windows amd64 / arm64。其它架构(386、riscv64、mips、s390x 等)请从源码构建:`CGO_ENABLED=0 go build ./cmd/agent`。

## 命令

| 命令 | 说明 |
| --- | --- |
| `nezha-agent -c config.yml` | 前台运行 |
| `nezha-agent edit -c config.yml` | 交互式编辑配置 |
| `nezha-agent service -c config.yml install` | 注册系统服务(systemd / OpenRC / launchd) |

字段说明见 [config.yaml.example](config.yaml.example);卸载服务用 `service ... uninstall`。

## 上游与许可

[nezhahq/agent](https://github.com/nezhahq/agent) · [Apache-2.0](LICENSE)
