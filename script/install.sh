#!/bin/sh
# ============================================================
# 哪吒监控 Agent 一键安装脚本(Linux / macOS)
#
# 安装(重复执行即升级/重新配置):
#   env NZ_SERVER=面板IP:8008 NZ_CLIENT_SECRET=你的密钥 sh install.sh
# 卸载:
#   sh install.sh uninstall
#
# 可选环境变量:
#   NZ_UUID     固定 Agent UUID(留空由 agent 首次启动时生成)
#   NZ_TLS      与面板通信启用 TLS,值 true/false(默认 false;公网部署务必 true)
#   NZ_VERSION  指定版本如 v1.2.3(默认 latest)
#   NZ_ALLOW_UNSIGNED=1  缺少 openssl 时跳过发布包验签(不推荐)
# ============================================================
set -e

REPO="wangdefaa/nezha-agent"
INSTALL_DIR="/opt/nezha/agent"
BIN="$INSTALL_DIR/nezha-agent"
CONFIG="$INSTALL_DIR/config.yml"
SVC="nezha-agent"
NL='
'
# 与 agent 自更新验签同一把公钥(cmd/agent/updater.go),私钥仅在发布流水线中
PUBKEY='-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEpXK/obH2gt//xnixHrPrhYJioB6x
zXzMbcmL9rUdVtCruCWWVj1FbMCvfryS8kxJdDNqt0DMpu9uw3rn5+IxTQ==
-----END PUBLIC KEY-----'

info() { echo ">> $*"; }
err()  { echo "错误: $*" >&2; exit 1; }
# 非 root 时借用 sudo;已是 root 直接执行
as_root() { if [ "$(id -u)" -ne 0 ]; then sudo "$@"; else "$@"; fi; }

# remove_service 用 init 系统原生命令停止并移除服务。不以 root 执行 $BIN:
# 安装后程序目录归 nezha 所有,二进制可能已被低权限进程替换。
remove_service() {
    if [ -f "/etc/systemd/system/$SVC.service" ]; then
        as_root systemctl disable --now "$SVC" >/dev/null 2>&1 || true
        as_root rm -f "/etc/systemd/system/$SVC.service"
        as_root systemctl daemon-reload >/dev/null 2>&1 || true
    elif [ -f "/etc/init.d/$SVC" ]; then
        as_root "/etc/init.d/$SVC" stop >/dev/null 2>&1 || true
        as_root rc-update del "$SVC" >/dev/null 2>&1 || true          # OpenRC
        as_root update-rc.d -f "$SVC" remove >/dev/null 2>&1 || true  # SysV(Debian)
        as_root chkconfig --del "$SVC" >/dev/null 2>&1 || true        # SysV(RHEL)
        as_root rm -f "/etc/init.d/$SVC"
    elif [ -f "/Library/LaunchDaemons/$SVC.plist" ]; then
        as_root launchctl unload "/Library/LaunchDaemons/$SVC.plist" >/dev/null 2>&1 || true
        as_root rm -f "/Library/LaunchDaemons/$SVC.plist"
    fi
    return 0
}

uninstall() {
    info "卸载 nezha-agent 服务..."
    remove_service
    as_root rm -rf "$INSTALL_DIR"
    info "已卸载。"
    exit 0
}

# check_params 校验必填参数;拒绝换行,防止向 YAML 注入额外配置项
check_params() {
    [ -n "$NZ_SERVER" ]        || err "请设置 NZ_SERVER(面板地址 host:port)"
    [ -n "$NZ_CLIENT_SECRET" ] || err "请设置 NZ_CLIENT_SECRET(Agent 密钥)"
    for v in "$NZ_SERVER" "$NZ_CLIENT_SECRET" "${NZ_UUID:-}" "${NZ_VERSION:-}"; do
        case "$v" in *"$NL"*) err "参数不能包含换行" ;; esac
    done
    case "${NZ_TLS:-false}" in true|false) ;; *) err "NZ_TLS 只能是 true 或 false" ;; esac
    [ "${NZ_TLS:-false}" = "true" ] || info "警告: 未启用 TLS,client_secret 将明文传输;公网部署请设 NZ_TLS=true"
}

# 当前发布的预编译目标(与发布流水线保持一致)
TARGETS="linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64"

# detect_platform 设置 os / arch;已下线的架构明确提示"不再提供",而不是去下载一个 404
detect_platform() {
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    case "$os" in linux|darwin) ;; *) err "不支持的系统: $os" ;; esac
    m=$(uname -m)
    case "$m" in
        x86_64|amd64)      arch=amd64 ;;
        aarch64|arm64)     arch=arm64 ;;
        armv7l|armv6l|arm) arch=arm ;;
        i386|i686|s390x|riscv64|mips|mipsle)
            err "架构 $m 已不再提供预编译包(当前提供: $TARGETS),请自行从源码编译" ;;
        *) err "不支持的架构: $m" ;;
    esac
    case " $TARGETS " in
        *" $os/$arch "*) ;;
        *) err "$os/$arch 不再提供预编译包(当前提供: $TARGETS)" ;;
    esac
}

# verify_zip 用内置公钥校验发布包签名,失败即中止
verify_zip() {
    if ! command -v openssl >/dev/null 2>&1; then
        [ "$NZ_ALLOW_UNSIGNED" = "1" ] || err "缺少 openssl,无法校验发布包签名(安装 openssl,或设 NZ_ALLOW_UNSIGNED=1 跳过)"
        info "警告: 已跳过发布包签名校验"
        return 0
    fi
    curl -fsSL "$url.sig" -o "$tmp/agent.zip.sig" || err "下载签名失败"
    printf '%s\n' "$PUBKEY" > "$tmp/pub.pem"
    openssl dgst -sha256 -verify "$tmp/pub.pem" -signature "$tmp/agent.zip.sig" "$tmp/agent.zip" >/dev/null 2>&1 \
        || err "发布包签名校验失败,已中止安装"
    info "发布包签名校验通过"
}

# download 下载、验签并以当前用户解压到临时目录
download() {
    ver="${NZ_VERSION:-latest}"
    if [ "$ver" = "latest" ]; then
        url="https://github.com/$REPO/releases/latest/download/nezha-agent_${os}_${arch}.zip"
    else
        url="https://github.com/$REPO/releases/download/${ver}/nezha-agent_${os}_${arch}.zip"
    fi
    info "下载 $url"
    curl -fsSL "$url" -o "$tmp/agent.zip" || err "下载失败,请检查网络或版本号"
    verify_zip
    unzip -o -q "$tmp/agent.zip" nezha-agent -d "$tmp/x" || err "解压失败"
}

# yaml_str 输出单引号 YAML 标量(内部单引号转义为两个单引号)
yaml_str() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/''/g")"; }

# install_files 收回目录属主并删除旧文件后再写入:旧目录归 nezha 所有,
# 预置的符号链接可诱导 root 覆写任意系统文件。配置以 0600 创建。
install_files() {
    as_root mkdir -p "$INSTALL_DIR"
    as_root chown 0:0 "$INSTALL_DIR"
    as_root rm -f "$BIN" "$CONFIG"
    as_root cp "$tmp/x/nezha-agent" "$BIN"
    as_root chmod 755 "$BIN"
    info "写入配置 $CONFIG"
    as_root sh -c "umask 077; cat > '$CONFIG'" <<EOF
server: $(yaml_str "$NZ_SERVER")
client_secret: $(yaml_str "$NZ_CLIENT_SECRET")
uuid: $(yaml_str "${NZ_UUID:-}")
tls: ${NZ_TLS:-false}
EOF
}

main() {
    [ "$1" = "uninstall" ] && uninstall
    for c in curl unzip; do command -v "$c" >/dev/null 2>&1 || err "缺少依赖: $c"; done
    detect_platform
    check_params
    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT
    trap 'exit 1' INT TERM
    download
    remove_service # 重复执行时先停掉旧服务,之后按新配置重新注册
    install_files
    # 二进制刚从已验签的包中写入且归 root 所有,此时以 root 执行是安全的
    info "注册并启动服务"
    as_root "$BIN" service -c "$CONFIG" install
    info "完成!查看状态: systemctl status $SVC(OpenRC: rc-service $SVC status;macOS: sudo launchctl list | grep $SVC)"
}

main "$@"
