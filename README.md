# 哪吒监控 Agent(精简改造版)

[![License: Apache 2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

本项目基于 [哪吒监控 Agent nezhahq/agent](https://github.com/nezhahq/agent) 改造,移除了命令执行、Web 终端、文件管理、NAT 穿透、GPU/温度采集等远程管控能力,只保留**采集与上报**职责,收敛权限、缩小被滥用面。

配合精简版面板 [wangdefaa/nezha](https://github.com/wangdefaa/nezha) 使用。

## 采集内容

- **系统**:CPU、内存、磁盘、负载、启动时间
- **网络**:实时上下行、累计流量、TCP/UDP 连接数
- **其它**:进程数、公网 IP / GeoIP
- **主动拨测**:TCP、ICMP(Ping)、HTTP

## 安装

### Linux / macOS

```bash
env NZ_SERVER=面板IP:8008 NZ_CLIENT_SECRET=你的密钥 \
  sh -c "$(curl -fsSL https://raw.githubusercontent.com/wangdefaa/nezha-agent/main/script/install.sh)"
```

卸载:`sh install.sh uninstall`。

### Windows(管理员 PowerShell)

```powershell
$env:NZ_SERVER="面板IP:8008"; $env:NZ_CLIENT_SECRET="你的密钥"
iwr -useb https://raw.githubusercontent.com/wangdefaa/nezha-agent/main/script/install.ps1 | iex
```

### Docker

见 [docker-compose.yaml](docker-compose.yaml)。Agent 采集宿主机指标,容器化需开启 host 网络与 PID 命名空间;完整宿主监控仍推荐用 install.sh 以 systemd 服务部署。

## 配置与命令

字段说明见 [config.yaml.example](config.yaml.example)。常用命令:

| 命令 | 说明 |
|---|---|
| `nezha-agent -c config.yml` | 前台运行 |
| `nezha-agent edit -c config.yml` | 交互式编辑配置 |
| `nezha-agent service -c config.yml install` | 注册为系统服务(systemd / OpenRC / launchd) |
| `nezha-agent service -c config.yml uninstall` | 卸载服务 |

## 上游与许可

- 基础项目:[哪吒监控 Agent nezhahq/agent](https://github.com/nezhahq/agent)
- 许可证:[Apache-2.0](LICENSE)
