# ============================================================
# 哪吒监控 Agent 安装脚本(Windows,需管理员 PowerShell)
#
# 安装:
#   $env:NZ_SERVER="面板IP:8008"; $env:NZ_CLIENT_SECRET="你的密钥"; .\install.ps1
# 卸载:
#   .\install.ps1 uninstall
#
# 可选环境变量:NZ_UUID、NZ_TLS(true/false)、NZ_VERSION(默认 latest)
# ============================================================
param([string]$Action = "install")
$ErrorActionPreference = "Stop"

$Repo       = "wangdefaa/nezha-agent"
$InstallDir = "C:\nezha\agent"
$Bin        = Join-Path $InstallDir "nezha-agent.exe"
$Config     = Join-Path $InstallDir "config.yml"

if ($Action -eq "uninstall") {
    if (Test-Path $Bin) { & $Bin service -c $Config uninstall }
    Remove-Item -Recurse -Force $InstallDir -ErrorAction SilentlyContinue
    Write-Host "已卸载。"
    exit 0
}

# 架构检测
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    "x86"   { "386" }
    default { throw "不支持的架构: $env:PROCESSOR_ARCHITECTURE" }
}

if (-not $env:NZ_SERVER)        { throw "请设置 `$env:NZ_SERVER(面板地址 host:port)" }
if (-not $env:NZ_CLIENT_SECRET) { throw "请设置 `$env:NZ_CLIENT_SECRET(Agent 密钥)" }

# 下载地址
$ver = if ($env:NZ_VERSION) { $env:NZ_VERSION } else { "latest" }
$url = if ($ver -eq "latest") {
    "https://github.com/$Repo/releases/latest/download/nezha-agent_windows_$arch.zip"
} else {
    "https://github.com/$Repo/releases/download/$ver/nezha-agent_windows_$arch.zip"
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$zip = Join-Path $env:TEMP "nezha-agent.zip"
Write-Host ">> 下载 $url"
Invoke-WebRequest -Uri $url -OutFile $zip
Expand-Archive -Path $zip -DestinationPath $InstallDir -Force
Remove-Item $zip

# 写配置
$uuid = if ($env:NZ_UUID) { $env:NZ_UUID } else { "" }
$tls  = if ($env:NZ_TLS)  { $env:NZ_TLS }  else { "false" }
@"
server: $($env:NZ_SERVER)
client_secret: $($env:NZ_CLIENT_SECRET)
uuid: $uuid
tls: $tls
"@ | Set-Content -Path $Config -Encoding UTF8

Write-Host ">> 注册并启动 Windows 服务"
& $Bin service -c $Config install
Write-Host "完成!"
