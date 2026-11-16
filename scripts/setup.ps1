$ErrorActionPreference = 'Stop'
$projectDir = Split-Path $PSScriptRoot -Parent
$envPath = Join-Path $projectDir '.env'
if (Test-Path -LiteralPath $envPath) {
    Write-Host '.env already exists; no changes made.'
    exit 0
}
function New-Secret {
    $bytes = New-Object byte[] 32
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
    return [Convert]::ToBase64String($bytes)
}
$text = Get-Content -Raw -LiteralPath (Join-Path $projectDir '.env.example')
$text = $text.Replace('replace-with-random-secret-at-least-32-characters', (New-Secret))
$text = $text.Replace('replace-with-another-random-secret-at-least-32-characters', (New-Secret))
[IO.File]::WriteAllText($envPath, $text, (New-Object Text.UTF8Encoding $false))
Write-Host 'Created .env. Set TELEGRAM_BOT_TOKEN and YANDEX_API_KEY, then run docker compose up --build -d.'
