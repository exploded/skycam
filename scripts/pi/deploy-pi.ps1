# deploy-pi.ps1 - build skycam for the Pi and install it.
#   .\scripts\pi\deploy-pi.ps1                       # to james@skycam.local
#   .\scripts\pi\deploy-pi.ps1 -Target pi@10.0.0.42
# The first run also provisions the Pi (packages, SDK, udev, user, service).
param([string]$Target = "james@skycam.local")
$ErrorActionPreference = "Stop"
$root = Resolve-Path "$PSScriptRoot\..\.."
$bundle = Join-Path $root "bin\skycam-bundle"

Remove-Item -Recurse -Force $bundle -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force $bundle | Out-Null

$saved = @{ CGO_ENABLED = $env:CGO_ENABLED; GOOS = $env:GOOS; GOARCH = $env:GOARCH }
try {
    $env:CGO_ENABLED = "0"; $env:GOOS = "linux"; $env:GOARCH = "arm64"
    Push-Location $root
    go build -ldflags="-s -w" -o "$bundle\skycam" ./cmd/skycam
    if ($LASTEXITCODE -ne 0) { throw "build failed" }
} finally {
    Pop-Location
    foreach ($k in $saved.Keys) { Set-Item "env:$k" $saved[$k] }
}

Copy-Item "$root\scripts\pi\setup.sh", "$root\scripts\pi\skycam.service", "$root\config.example.json" $bundle
Copy-Item "$root\third_party\zwo\libASICamera2.so.1.41", "$root\third_party\zwo\asi.rules" $bundle

# Unix line endings, or bash on the Pi chokes on the \r.
foreach ($f in "setup.sh", "skycam.service") {
    $p = Join-Path $bundle $f
    [IO.File]::WriteAllText($p, ([IO.File]::ReadAllText($p) -replace "`r`n", "`n"))
}

ssh $Target "rm -rf /tmp/skycam-bundle"
scp -r $bundle "${Target}:/tmp/skycam-bundle"
if ($LASTEXITCODE -ne 0) { throw "copy failed" }
ssh -t $Target "sudo bash /tmp/skycam-bundle/setup.sh /tmp/skycam-bundle"
if ($LASTEXITCODE -ne 0) { throw "install failed" }
