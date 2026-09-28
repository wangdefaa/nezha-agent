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
$BaseDir    = "C:\nezha"
$InstallDir = Join-Path $BaseDir "agent"
$Bin        = Join-Path $InstallDir "nezha-agent.exe"
$Config     = Join-Path $InstallDir "config.yml"

# 停止并删除指向安装目录的服务。用 sc.exe 而不是以管理员身份执行 $Bin:
# 旧版安装目录继承了 C:\ 的 "Authenticated Users: 修改" 权限,二进制可能已被替换。
function Remove-AgentService {
    Get-CimInstance Win32_Service |
        Where-Object { $_.PathName -like "*$InstallDir*" } |
        ForEach-Object {
            Stop-Service -Name $_.Name -Force -ErrorAction SilentlyContinue  # 同步等待停止,释放 exe 文件锁
            & sc.exe delete $_.Name | Out-Null
        }
}

# 收紧目录 ACL:C:\ 下新建目录会继承 "Authenticated Users: 修改",任何本地用户都能
# 替换以 LocalSystem 运行的服务二进制(提权)或读取含 client_secret 的配置。
# 这里断开继承,只授予 SYSTEM(S-1-5-18)与 Administrators(S-1-5-32-544)完全控制。
function Protect-AgentDir {
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    & icacls.exe $BaseDir /inheritance:r /grant:r "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "设置目录权限失败: $BaseDir" }
    & icacls.exe $InstallDir /reset /T /C /Q | Out-Null
}

# 按 checksums.txt 校验 sha256。注意:它与发布包同源,只能发现传输/镜像损坏,
# 不能抵御发布源被篡改;签名(.sig)校验需另行实现。
function Test-AgentZip([string]$Zip, [string]$Name, [string]$SumUrl) {
    $sumFile = "$Zip.sums"  # 落盘再读:octet-stream 响应在 PS 5.1 中 .Content 为 byte[]
    Invoke-WebRequest -UseBasicParsing -Uri $SumUrl -OutFile $sumFile
    $sums = Get-Content -Path $sumFile
    Remove-Item -Force $sumFile
    $line = $sums | Where-Object { $_ -match "\s$([regex]::Escape($Name))\s*$" } | Select-Object -First 1
    if (-not $line) { throw "checksums.txt 中未找到 $Name" }
    $want = ($line -split '\s+')[0].ToLower()
    $got  = (Get-FileHash -Algorithm SHA256 -Path $Zip).Hash.ToLower()
    if ($want -ne $got) { throw "sha256 校验失败: $Name" }
}

if ($Action -eq "uninstall") {
    Remove-AgentService
    Remove-Item -Recurse -Force $InstallDir -ErrorAction SilentlyContinue
    Write-Host "已卸载。"
    exit 0
}

# 架构检测:当前只发布 windows amd64/arm64。64 位系统上的 32 位 PowerShell 中
# PROCESSOR_ARCHITECTURE 为 x86,真实架构在 PROCESSOR_ARCHITEW6432。
$procArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$arch = switch ($procArch) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    "x86"   { throw "windows 386(32 位)已不再提供预编译包(当前提供 windows amd64/arm64)" }
    default { throw "不支持的架构: $procArch" }
}

if (-not $env:NZ_SERVER)        { throw "请设置 `$env:NZ_SERVER(面板地址 host:port)" }
if (-not $env:NZ_CLIENT_SECRET) { throw "请设置 `$env:NZ_CLIENT_SECRET(Agent 密钥)" }
foreach ($v in @($env:NZ_SERVER, $env:NZ_CLIENT_SECRET, $env:NZ_UUID)) {
    if ($v -match "[`r`n]") { throw "参数不能包含换行" }
}
$tls = if ($env:NZ_TLS) { $env:NZ_TLS } else { "false" }
if ($tls -notin @("true", "false")) { throw "NZ_TLS 只能是 true 或 false" }

# 下载地址
$ver  = if ($env:NZ_VERSION) { $env:NZ_VERSION } else { "latest" }
$base = if ($ver -eq "latest") { "https://github.com/$Repo/releases/latest/download" } else { "https://github.com/$Repo/releases/download/$ver" }
$name = "nezha-agent_windows_$arch.zip"

Remove-AgentService
Protect-AgentDir
# 下载到受保护目录内的随机文件名,避免 %TEMP%(以 SYSTEM 运行时为 C:\Windows\Temp)被抢先替换
$zip = Join-Path $InstallDir ("download-" + [guid]::NewGuid().ToString("N") + ".zip")
Write-Host ">> 下载 $base/$name"
Invoke-WebRequest -UseBasicParsing -Uri "$base/$name" -OutFile $zip
try {
    Test-AgentZip $zip $name "$base/checksums.txt"
    Expand-Archive -Path $zip -DestinationPath $InstallDir -Force
} finally {
    Remove-Item -Force $zip -ErrorAction SilentlyContinue
}

# 写配置(单引号 YAML 标量,内部单引号转义为两个单引号)
function ConvertTo-YamlString([string]$s) { "'" + $s.Replace("'", "''") + "'" }
$uuid = if ($env:NZ_UUID) { $env:NZ_UUID } else { "" }
@"
server: $(ConvertTo-YamlString $env:NZ_SERVER)
client_secret: $(ConvertTo-YamlString $env:NZ_CLIENT_SECRET)
uuid: $(ConvertTo-YamlString $uuid)
tls: $tls
"@ | Set-Content -Path $Config -Encoding UTF8

Write-Host ">> 注册并启动 Windows 服务"
& $Bin service -c $Config install
Write-Host "完成!"
