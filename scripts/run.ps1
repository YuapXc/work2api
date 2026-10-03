# work2api 本地构建 + 运行（免费，无需签名/CI）。
#
#   pwsh -File scripts/run.ps1            # 构建 work2api.exe 并运行
#   pwsh -File scripts/run.ps1 -Sign      # 额外做本地自签名（减少"未知发布者"提示）
#   pwsh -File scripts/run.ps1 -NoRun     # 只构建不运行
#   pwsh -File scripts/run.ps1 -NoBrowser # 启动但不自动打开管理面板
#
# 说明：用固定路径的 work2api.exe，而不是 `go run`（后者编译到临时目录再执行，
# 更容易被火绒等 AV 的启发式拦截）。首次仍可能被报 Trojan/Intercept.a（误报）——
# 把本项目目录或 work2api.exe 加入火绒「信任区」即可（见 README/HANDOFF）。

param(
  [switch]$Sign,
  [switch]$NoRun,
  [switch]$NoBrowser
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

# Reuse this checkout's running process before attempting to overwrite its executable.
$exePath = Join-Path $root 'work2api.exe'
$existing = Get-Process -Name work2api -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exePath } | Select-Object -First 1
if ($existing) {
  if ($NoRun -or $Sign) { throw '本项目服务正在运行，请先关闭服务再构建或签名。' }
  $listener = Get-NetTCPConnection -State Listen -OwningProcess $existing.Id -ErrorAction SilentlyContinue | Select-Object -First 1
  if (-not $listener) { throw '本项目服务正在启动，请稍后重试。' }
  $existingURL = "http://localhost:$($listener.LocalPort)/admin-ui/"
  Write-Host "服务已运行 (PID $($existing.Id))，管理面板: $existingURL" -ForegroundColor Green
  if (-not $NoBrowser) { Start-Process $existingURL }
  return
}

$localPort = 8787
$portText = $env:PORT
if (-not $portText -and (Test-Path -LiteralPath (Join-Path $root '.env'))) {
  foreach ($line in Get-Content -LiteralPath (Join-Path $root '.env')) {
    if ($line -match '^\s*PORT\s*=\s*(.*?)\s*$') { $portText = $Matches[1].Trim().Trim('"').Trim("'") }
  }
}
if ($portText) {
  if (-not [int]::TryParse($portText, [ref]$localPort) -or $localPort -lt 1 -or $localPort -gt 65535) { throw 'PORT 必须为 1–65535 的整数' }
}
if (-not $NoRun -and (Get-NetTCPConnection -State Listen -LocalPort $localPort -ErrorAction SilentlyContinue)) {
  throw "端口 $localPort 被其他服务占用，请配置 PORT 后再启动。"
}

# 保证 go 在 PATH（未加入系统 PATH 时用默认安装路径）
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
  $env:Path += ";C:\Program Files\Go\bin"
}

# 版本号取自 git（tag 优先，否则短 commit）
$version = (git describe --tags --always 2>$null)
if (-not $version) { $version = 'dev' }

Write-Host "构建 work2api.exe ($version) ..." -ForegroundColor Cyan
$env:CGO_ENABLED = '0'
# 把 Go 链接器的临时目录指到仓库内（默认在 %Temp%\go-build*，火绒会拦截/锁定链接器
# 刚产出的 a.out.exe，导致 "Access is denied" 构建失败）。放到项目盘可绕开。
$tmp = Join-Path $root '.gobuildtmp'
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
$env:GOTMPDIR = $tmp
go build -trimpath -ldflags "-X main.version=$version" -o work2api.exe ./cmd/server
if ($LASTEXITCODE -ne 0) { throw "go build 失败" }
Write-Host "构建完成: $root\work2api.exe" -ForegroundColor Green

if ($Sign) {
  $cert = Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert -ErrorAction SilentlyContinue |
          Where-Object { $_.Subject -eq 'CN=work2api-dev' } | Select-Object -First 1
  if (-not $cert) {
    Write-Host "创建本地自签名证书 CN=work2api-dev ..." -ForegroundColor Cyan
    $cert = New-SelfSignedCertificate -Type CodeSigningCert -Subject 'CN=work2api-dev' `
      -CertStoreLocation Cert:\CurrentUser\My -KeyUsage DigitalSignature `
      -FriendlyName 'work2api dev signing'
  }
  Set-AuthenticodeSignature -FilePath .\work2api.exe -Certificate $cert `
    -HashAlgorithm SHA256 -TimestampServer 'http://timestamp.digicert.com' | Out-Null
  $st = (Get-AuthenticodeSignature .\work2api.exe).Status
  Write-Host "已自签名，状态: $st（自签名不产生云端信誉，仅去掉未知发布者提示）" -ForegroundColor Green
}

if (-not $NoRun) {
  $browserHelper = $null
  if (-not $NoBrowser) {
    $adminURL = "http://localhost:$localPort/admin-ui/"
    Write-Host "管理面板: $adminURL（服务就绪后自动打开）" -ForegroundColor Cyan
    $browserCode = @"
`$deadline = [DateTime]::UtcNow.AddSeconds(30)
while ([DateTime]::UtcNow -lt `$deadline) {
  try {
    `$response = Invoke-WebRequest -Uri '$adminURL' -UseBasicParsing -TimeoutSec 2
    if (`$response.StatusCode -eq 200) { Start-Process '$adminURL'; break }
  } catch {}
  Start-Sleep -Milliseconds 500
}
"@
    $encodedBrowserCode = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($browserCode))
    $browserHelper = Start-Process -FilePath (Get-Process -Id $PID).Path `
      -ArgumentList '-NoProfile', '-NonInteractive', '-EncodedCommand', $encodedBrowserCode `
      -WindowStyle Hidden -PassThru
  }
  Write-Host "启动 (Ctrl+C 退出) ..." -ForegroundColor Cyan
  try { & .\work2api.exe -port $localPort -strict-port }
  finally {
    if ($browserHelper -and -not $browserHelper.HasExited) { $browserHelper.Kill() }
  }
}