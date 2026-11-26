# ============================================
# esp-relay - Build + Deploy no servidor (Windows, via SSH)
# ============================================
# Compila (vet + testes + build) e publica o esp-relay-gateway.exe no servidor,
# reiniciando o servico. O painel vai embutido no exe: nada de html para copiar.
#
# Uso, na pasta gateway:
#   .\deploy.ps1 -RemoteHost servidor.exemplo.com -RemoteUser usuario `
#                -Key C:\caminho\id_ed25519 -AppDir D:\apps\esp-relay
#   .\deploy.ps1 ... -SkipTests       # sem vet/testes
#   .\deploy.ps1 ... -NoBuild         # envia o dist\ existente
#   .\deploy.ps1 ... -WhatIf          # mostra o que faria, sem enviar nada
#
# Requer: chave SSH do usuario, e o ssh.exe/scp.exe do Windows (ou OpenSSH).
# O servidor precisa ter um servico Windows (padrao: esp-relay) apontando para
# o .exe em -AppDir; veja o README do gateway para o modelo de servico.
# ============================================
param(
    [Parameter(Mandatory = $true)][string]$Key,
    [Parameter(Mandatory = $true)][string]$RemoteHost,
    [Parameter(Mandatory = $true)][string]$RemoteUser,
    [Parameter(Mandatory = $true)][string]$AppDir,
    [int]$SshPort       = 22,
    [string]$Service    = "esp-relay",
    [int]$HealthPort    = 8080,
    [switch]$SkipTests,
    [switch]$NoBuild,
    # Mostra o que seria feito, sem enviar nem reiniciar nada.
    [switch]$WhatIf
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

if (-not (Test-Path $Key)) {
    throw "Chave SSH nao encontrada: $Key"
}
if (-not (Get-Command ssh -ErrorAction SilentlyContinue)) {
    throw "ssh.exe nao encontrado. No Windows, habilite o 'OpenSSH Client' em Recursos Opcionais."
}

$target = "${RemoteUser}@${RemoteHost}"
$exe    = Join-Path "dist" "esp-relay-gateway.exe"

# --- 1. Build ---
if (-not $NoBuild) {
    $versionTimestamp = [DateTimeOffset]::UtcNow.ToOffset([TimeSpan]::FromHours(-3))
    $version = $versionTimestamp.ToString("yyyyMMdd'T'HHmmssfffzzz").Replace(":", "") +
        "-" + [guid]::NewGuid().ToString("N").Substring(0, 8)
    Write-Host "Versao : $version"
    & .\build.ps1 -SkipTests:$SkipTests -Version $version
    if ($LASTEXITCODE -ne 0) { throw "Build falhou; deploy abortado." }
} else {
    Write-Host "Build  : pulado; sera mantida a versao embutida no binario existente."
}
if (-not (Test-Path $exe)) {
    throw "Binario nao encontrado: $exe. Rode .\build.ps1 antes."
}

$size = [math]::Round((Get-Item $exe).Length / 1MB, 2)
Write-Host ""
Write-Host "========================================"
Write-Host " Deploy -> $target"
Write-Host "========================================"
Write-Host "Binario : $exe ($size MB)"
Write-Host "Destino : $AppDir"
Write-Host ""

if ($WhatIf) {
    Write-Host "[WhatIf] Nada foi enviado. Seria executado:"
    Write-Host "  1. ssh $target 'mkdir C:\temp'"
    Write-Host "  2. scp $exe -> ${target}:C:/temp/esp-relay-gateway.exe"
    Write-Host "  3. no servidor: Stop-Service $Service; Copy-Item para $AppDir; Start-Service $Service"
    Write-Host "  4. conferir /health na porta $HealthPort"
    exit 0
}

# --- 2. Enviar para C:\temp (o exe em execucao fica travado no Windows,
#        entao a troca acontece no servidor, com o servico parado) ---
Write-Host "[1/3] Enviando binario..."
# O shell do sshd varia (cmd ou PowerShell): um comando unico, valido nos dois,
# sem &, | nem parenteses. mkdir numa pasta que ja existe e erro em ambos, entao
# a existencia e conferida antes e o erro do scp diz o resto.
& ssh -i $Key -p $SshPort -o StrictHostKeyChecking=accept-new $target "powershell -NoProfile -Command New-Item -ItemType Directory -Force C:\temp | Out-Null"
if ($LASTEXITCODE -ne 0) { throw "Falha no SSH (conexao/chave?)." }

& scp -i $Key -P $SshPort -o StrictHostKeyChecking=accept-new $exe "${target}:C:/temp/esp-relay-gateway.exe"
if ($LASTEXITCODE -ne 0) { throw "Falha no scp ao enviar o binario." }

# --- 3. Trocar e reiniciar. Vai como um .ps1 temporario para nao sofrer com
#        o escape de aspas do "ssh ... powershell -Command". ---
Write-Host "[2/3] Trocando binario e reiniciando servico..."
$remoteScript = @'
$ErrorActionPreference = "Stop"
Stop-Service "__SERVICE__" -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1
Copy-Item "C:\temp\esp-relay-gateway.exe" "__APPDIR__\esp-relay-gateway.exe" -Force
Remove-Item "C:\temp\esp-relay-gateway.exe" -Force
Start-Service "__SERVICE__"
Start-Sleep -Seconds 2
$s = Get-Service "__SERVICE__"
Write-Output ("status=" + $s.Status)
$h = Invoke-RestMethod "http://127.0.0.1:__HEALTHPORT__/health" -TimeoutSec 5
Write-Output ("health=" + $h.status)
'@
$remoteScript = $remoteScript.Replace("__SERVICE__", $Service).
    Replace("__APPDIR__", $AppDir).
    Replace("__HEALTHPORT__", "$HealthPort")

$localTmp = Join-Path $env:TEMP "esp-relay-deploy-remote.ps1"
Set-Content -Path $localTmp -Value $remoteScript -Encoding ASCII

& scp -i $Key -P $SshPort -o StrictHostKeyChecking=accept-new $localTmp "${target}:C:/temp/esp-relay-deploy.ps1"
if ($LASTEXITCODE -ne 0) { throw "Falha no scp do script remoto." }

$result = & ssh -i $Key -p $SshPort $target "powershell -NoProfile -ExecutionPolicy Bypass -File C:\temp\esp-relay-deploy.ps1; del C:\temp\esp-relay-deploy.ps1"
Remove-Item $localTmp -Force -ErrorAction SilentlyContinue
if ($LASTEXITCODE -ne 0) { throw "Falha ao trocar o binario/reiniciar o servico." }

Write-Host "[3/3] Resultado:"
$result | ForEach-Object { Write-Host "  $_" }

$statusLine = $result | Where-Object { $_ -like "status=*" }
$healthLine = $result | Where-Object { $_ -like "health=*" }
if ($statusLine -notmatch "Running") {
    Write-Host ""
    Write-Host "ATENCAO: o servico nao esta Running." -ForegroundColor Yellow
    Write-Host "  ssh -i `"$Key`" -p $SshPort $target `"Get-WinEvent -LogName Application -MaxEvents 20`""
    exit 1
}
if ($healthLine -notmatch "ok") {
    Write-Host ""
    Write-Host "ATENCAO: /health nao respondeu ok (porta $HealthPort)." -ForegroundColor Yellow
    exit 1
}

Write-Host ""
Write-Host "Deploy concluido."
