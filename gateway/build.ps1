# ============================================
# esp-relay - Build do binario Windows
# ============================================
# Gera esp-relay-gateway.exe (painel embutido via go:embed: NAO precisa copiar
# nenhum .html junto).
#
# Uso, na pasta gateway:
#   .\build.ps1              # testes + vet + build
#   .\build.ps1 -SkipTests   # so o build (atalho)
#   .\build.ps1 -OutDir C:\temp\esp-relay
#
# Saida: .\dist\esp-relay-gateway.exe
# ============================================
param(
    [string]$OutDir = "dist",
    [string]$Version = "",
    [switch]$SkipTests
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "go.exe nao encontrado no PATH. Instale o Go (1.22+) e abra um novo terminal."
}

if ([string]::IsNullOrWhiteSpace($Version)) {
    $versionTimestamp = [DateTimeOffset]::UtcNow.ToOffset([TimeSpan]::FromHours(-3))
    $Version = $versionTimestamp.ToString("yyyyMMdd'T'HHmmssfffzzz").Replace(":", "") +
        "-" + [guid]::NewGuid().ToString("N").Substring(0, 8)
}
if ($Version -notmatch '^[A-Za-z0-9._-]+$') {
    throw "Versao invalida: use apenas letras, numeros, ponto, hifen e underscore."
}

# -s -w remove símbolos e debug info; -trimpath omite caminhos locais do build.
$ldflags = "-s -w -X main.buildVersion=$Version"

Write-Host "========================================"
Write-Host " esp-relay - Build"
Write-Host "========================================"
Write-Host "Go      : $((go version))"
Write-Host "Saida   : $OutDir\esp-relay-gateway.exe"
Write-Host "Versao  : $Version"
Write-Host "Painel  : embutido (web/index.html via go:embed)"
Write-Host ""

if (-not $SkipTests) {
    Write-Host "[1/3] go vet..."
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw "go vet falhou; build abortado." }

    Write-Host "[2/3] go test..."
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw "Testes falharam; build abortado." }
} else {
    Write-Host "[1/3] -(vet) pulado (-SkipTests)"
    Write-Host "[2/3] -(test) pulado (-SkipTests)"
}

Write-Host "[3/3] Compilando..."
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$exe = Join-Path $OutDir "esp-relay-gateway.exe"

$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -trimpath -ldflags $ldflags -o $exe .

if ($LASTEXITCODE -ne 0 -or -not (Test-Path $exe)) {
    throw "Falha ao compilar."
}

$info = Get-Item $exe
Write-Host ""
Write-Host "========================================"
Write-Host " Build concluido"
Write-Host "========================================"
Write-Host " Arquivo : $($info.FullName)"
Write-Host " Tamanho : $([math]::Round($info.Length / 1MB, 2)) MB"
Write-Host " Data    : $($info.LastWriteTime)"
Write-Host ""
Write-Host "Para publicar, copie o binario para o servidor e reinicie o serviço"
Write-Host "(ou use .\deploy.ps1). O painel vai embutido: nada de .html para levar."
