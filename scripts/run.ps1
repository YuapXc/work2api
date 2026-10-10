# work2api 本地构建 + 运行（免费，无需签名/CI）。
#
#   pwsh -File scripts/run.ps1            # 构建 work2api.exe 并运行
#   pwsh -File scripts/run.ps1 -Sign      # 额外做本地自签名（减少"未知发布者"提示）
#   pwsh -File scripts/run.ps1 -NoRun     # 只构建不运行
#   pwsh -File scripts/run.ps1 -NoBrowser # 启动但不自动打开管理面板
#   pwsh -File scripts/run.ps1 -Restart   # 确认后排空请求并重启/更新
#
# 说明：用固定路径的 work2api.exe，而不是 `go run`（后者编译到临时目录再执行，
# 更容易被火绒等 AV 的启发式拦截）。首次仍可能被报 Trojan/Intercept.a（误报）——
# 把本项目目录或 work2api.exe 加入火绒「信任区」即可（见 README/HANDOFF）。

param(
  [switch]$Sign,
  [switch]$NoRun,
  [switch]$NoBrowser,
  [switch]$Restart
)

$ErrorActionPreference = 'Stop'
try {
Add-Type -AssemblyName System.Net.Http
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

# Fingerprint actual Go build inputs, including uncommitted code and embedded UI.
function Get-BuildFingerprint {
  $hash = [Security.Cryptography.IncrementalHash]::CreateHash([Security.Cryptography.HashAlgorithmName]::SHA256)
  try {
    $inputs = @('go.mod', 'go.sum')
    $inputs += Get-ChildItem cmd, internal -Recurse -File | Where-Object { $_.Extension -eq '.go' -and $_.Name -notlike '*_test.go' -or $_.FullName.StartsWith((Join-Path $root 'internal\app\webui') + '\') } | ForEach-Object { $_.FullName.Substring($root.Length + 1).Replace('\', '/') }
    # .NET Framework and modern .NET use different culture sorting rules.
    # Build identity must remain identical across both PowerShell hosts.
    [string[]]$orderedInputs = $inputs
    [Array]::Sort($orderedInputs, [StringComparer]::Ordinal)
    foreach ($inputFile in $orderedInputs) {
      $hash.AppendData([Text.Encoding]::UTF8.GetBytes($inputFile + "`n"))
      $hash.AppendData([IO.File]::ReadAllBytes((Join-Path $root $inputFile)))
      $hash.AppendData([byte[]]@(0))
    }
    return [BitConverter]::ToString($hash.GetHashAndReset()).Replace('-', '').ToLowerInvariant()
  } finally { $hash.Dispose() }
}
$fingerprint = Get-BuildFingerprint
$exePath = Join-Path $root 'work2api.exe'
$existing = Get-Process -Name work2api -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exePath } | Select-Object -First 1
$updating = $false
if ($existing) {
  if ($NoRun -or $Sign) { throw '本项目服务正在运行，请先正常退出，再仅构建或签名。' }
  # An old executable cannot safely be queried using flags it does not support.
  $dataPath = $env:DATA_DIR
  if (-not $dataPath -and (Test-Path -LiteralPath (Join-Path $root '.env'))) {
    foreach ($line in Get-Content -LiteralPath (Join-Path $root '.env')) {
      if ($line -match '^\s*DATA_DIR\s*=\s*(.*?)\s*$') { $dataPath = $Matches[1].Trim().Trim('"').Trim("'") }
    }
  }
  if (-not $dataPath) { $dataPath = 'data' }
  if (-not [IO.Path]::IsPathRooted($dataPath)) { $dataPath = Join-Path $root $dataPath }
  if (-not (Test-Path -LiteralPath (Join-Path $dataPath '.instance.json'))) { throw '旧服务没有安全退出控制接口，或仍在启动。请稍后重试；旧版本首次升级需先正常退出。' }
  # Stale metadata alone does not prove that this running binary supports these flags.
  # Authenticate its live control endpoint and match the actual process first.
  $metadata = Get-Content -LiteralPath (Join-Path $dataPath '.instance.json') -Raw | ConvertFrom-Json
  $controlUri = $null
  if (-not [Uri]::TryCreate($metadata.control, [UriKind]::Absolute, [ref]$controlUri) -or
      $controlUri.Scheme -ne 'http' -or $controlUri.Host -ne '127.0.0.1' -or
      $controlUri.AbsolutePath -ne '/instance' -or $controlUri.UserInfo -or $controlUri.Query -or $controlUri.Fragment -or
      $metadata.application -ne 'work2api' -or $metadata.pid -ne $existing.Id -or $metadata.token -notmatch '^[0-9a-f]{64}$') {
    throw '实例元数据不匹配运行进程，未调用旧程序控制参数。'
  }
  $probeHandler = [Net.Http.HttpClientHandler]::new()
  $probeHandler.UseProxy = $false
  $probeHandler.AllowAutoRedirect = $false
  $probeClient = [Net.Http.HttpClient]::new($probeHandler)
  try {
    $probeClient.Timeout = [TimeSpan]::FromSeconds(3)
    $probeClient.DefaultRequestHeaders.Authorization = [Net.Http.Headers.AuthenticationHeaderValue]::new('Bearer', $metadata.token)
    $probeResponse = $probeClient.GetAsync($controlUri).GetAwaiter().GetResult()
    try {
      if ([int]$probeResponse.StatusCode -ne 200) { throw '已有实例控制接口尚未就绪或正在退出。' }
      $live = $probeResponse.Content.ReadAsStringAsync().GetAwaiter().GetResult() | ConvertFrom-Json
      if ($live.application -ne 'work2api' -or $live.pid -ne $existing.Id -or $live.identity -ne $metadata.identity -or
          $live.control -ne $metadata.control -or $live.build -ne $metadata.build -or $live.admin_url -ne $metadata.admin_url) {
        throw '运行实例身份校验失败，未调用旧程序控制参数。'
      }
    } finally { $probeResponse.Dispose() }
  } finally { $probeClient.Dispose() }
  # Probe identity and liveness before offering an update; no process is killed.
  & $exePath -reuse-only
  if ($LASTEXITCODE -ne 0) { throw '无法确认已有实例，不进行替换或退出操作。' }
  $built = & $exePath -build-info | ConvertFrom-Json
  if ($LASTEXITCODE -ne 0) { throw '无法读取运行版本。' }
  if ($built.build -eq $fingerprint -and -not $Restart) {
    if (-not $NoBrowser) { & $exePath -reuse-only -open-browser }
    return
  }
  Write-Host '检测到代码变化或已请求重启。将先构建，再等待已有请求结束；期间新请求需稍后重试。' -ForegroundColor Yellow
  $updateAnswer = Read-Host '现在平滑更新/重启？输入 Y 确认，其他输入继续复用旧服务'
  if ($updateAnswer -notin @('Y', 'y')) {
    if (-not $NoBrowser) { & $exePath -reuse-only -open-browser }
    return
  }
  $updating = $true
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
if (-not $NoRun -and -not $updating -and (Get-NetTCPConnection -State Listen -LocalPort $localPort -ErrorAction SilentlyContinue)) {
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
$buildTarget = if ($updating) { Join-Path $root 'work2api.next.exe' } else { $exePath }
go build -trimpath -ldflags "-X main.version=$version -X main.buildFingerprint=$fingerprint" -o $buildTarget ./cmd/server
if ($LASTEXITCODE -ne 0) { throw "go build 失败" }
if ((Get-BuildFingerprint) -ne $fingerprint) { throw '构建期间源文件发生变化，请重新启动；旧服务未停止。' }
Write-Host "构建完成: $buildTarget" -ForegroundColor Green
if ($updating) {
  $candidate = & $buildTarget -build-info | ConvertFrom-Json
  if ($LASTEXITCODE -ne 0 -or $candidate.build -ne $fingerprint) { throw '新版构建验证失败，旧服务继续运行。' }
  Write-Host '等待旧服务中已有请求结束（最多 60 秒；超时恢复旧服务）...' -ForegroundColor Cyan
  & $exePath -stop-running
  if ($LASTEXITCODE -ne 0) { throw '未确认旧服务退出，未替换程序。已构建的 work2api.next.exe 保留。' }
  if (-not $existing.WaitForExit(80000)) { throw '旧进程尚未退出，未替换程序，请核实状态。' }
  # Keep the prior executable for diagnosis/manual rollback. Never move runtime data.
  Copy-Item -LiteralPath $exePath -Destination (Join-Path $root 'work2api.previous.exe') -Force
  Copy-Item -LiteralPath $buildTarget -Destination $exePath -Force
}

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
  Write-Host "启动 (Ctrl+C 正常退出) ..." -ForegroundColor Cyan
  $runArgs = @('-port', "$localPort", '-strict-port')
  if (-not $NoBrowser) { $runArgs += '-open-browser' }
  & $exePath @runArgs
  if ($LASTEXITCODE -ne 0) { throw '服务启动失败；未自动回滚数据库，请查看报错。' }
}
} catch {
  Write-Host ("启动失败：" + $_.Exception.Message) -ForegroundColor Red
  Write-Host $_.InvocationInfo.PositionMessage -ForegroundColor DarkGray
  if (-not $NoRun -and $env:WORK2API_LAUNCHER -ne 'cmd' -and -not [Console]::IsInputRedirected) {
    Read-Host '按 Enter 关闭窗口' | Out-Null
  }
  exit 1
}
