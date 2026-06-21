#!/bin/sh
# ============================================================
# 哪吒监控 Agent 一键安装脚本(Linux / macOS)
#
# 安装:
#   env NZ_SERVER=面板IP:8008 NZ_CLIENT_SECRET=你的密钥 sh install.sh
# 卸载:
#   sh install.sh uninstall
#
# 可选环境变量:
#   NZ_UUID     固定 Agent UUID(留空由面板分配)
#   NZ_TLS      与面板通信启用 TLS,值 true/false(默认 false)
#   NZ_VERSION  指定版本如 v1.2.3(默认 latest)
# ============================================================
set -e

REPO="wangdefaa/nezha-agent"
INSTALL_DIR="/opt/nezha/agent"
BIN="$INSTALL_DIR/nezha-agent"
CONFIG="$INSTALL_DIR/config.yml"

info() { echo ">> $*"; }
err()  { echo "错误: $*" >&2; exit 1; }
# 非 root 时借用 sudo;已是 root 直接执行
as_root() { if [ "$(id -u)" -ne 0 ]; then sudo "$@"; else "$@"; fi; }

uninstall() {
    info "卸载 nezha-agent 服务..."
    [ -x "$BIN" ] && as_root "$BIN" service -c "$CONFIG" uninstall 2>/dev/null || true
    as_root rm -rf "$INSTALL_DIR"
    info "已卸载。"
    exit 0
}
[ "$1" = "uninstall" ] && uninstall

# 1. 依赖
for c in curl unzip; do command -v "$c" >/dev/null 2>&1 || err "缺少依赖: $c"; done

# 2. 平台检测
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in linux|darwin) ;; *) err "不支持的系统: $os" ;; esac
case "$(uname -m)" in
    x86_64|amd64)      arch=amd64 ;;
    i386|i686)         arch=386 ;;
    aarch64|arm64)     arch=arm64 ;;
    armv7l|armv6l|arm) arch=arm ;;
    s390x)             arch=s390x ;;
    riscv64)           arch=riscv64 ;;
    mips)              arch=mips ;;
    mipsle)            arch=mipsle ;;
    *) err "不支持的架构: $(uname -m)" ;;
esac

# 3. 必填参数
[ -n "$NZ_SERVER" ]        || err "请设置 NZ_SERVER(面板地址 host:port)"
[ -n "$NZ_CLIENT_SECRET" ] || err "请设置 NZ_CLIENT_SECRET(Agent 密钥)"

# 4. 下载并解压
ver="${NZ_VERSION:-latest}"
if [ "$ver" = "latest" ]; then
    url="https://github.com/$REPO/releases/latest/download/nezha-agent_${os}_${arch}.zip"
else
    url="https://github.com/$REPO/releases/download/${ver}/nezha-agent_${os}_${arch}.zip"
fi
info "下载 $url"
tmp=$(mktemp -d)
curl -fsSL "$url" -o "$tmp/agent.zip" || err "下载失败,请检查网络或版本号"
as_root mkdir -p "$INSTALL_DIR"
as_root unzip -o "$tmp/agent.zip" -d "$INSTALL_DIR" >/dev/null
as_root chmod +x "$BIN"
rm -rf "$tmp"

# 5. 写配置(其余字段用默认值,可后续 `nezha-agent edit` 修改)
info "写入配置 $CONFIG"
as_root sh -c "cat > '$CONFIG'" <<EOF
server: $NZ_SERVER
client_secret: $NZ_CLIENT_SECRET
uuid: ${NZ_UUID:-}
tls: ${NZ_TLS:-false}
EOF

# 6. 安装并启动服务(systemd / OpenRC / launchd 由 service 库自动适配)
info "注册并启动服务"
as_root "$BIN" service -c "$CONFIG" install

info "完成!查看状态:  $BIN service -c $CONFIG status"
