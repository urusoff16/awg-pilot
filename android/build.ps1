# Builds AWG Pilot for Android without Gradle: Go core (NDK) -> web UI (Vite) -> aapt2 -> javac -> d8 -> zip -> zipalign -> apksigner.
# Output: build\android\awgpilot-<version>.apk
param(
    [string]$Version = "0.9.1",
    [int]$VersionCode = 901,
    [string[]]$Abis = @("arm64-v8a", "armeabi-v7a", "x86_64"),
    [string]$SigningDir = "D:\CLAUDE_CODE\tools\awgpilot-signing"
)
$ErrorActionPreference = "Stop"

$root = Split-Path $PSScriptRoot -Parent
$sdk = Join-Path $env:LOCALAPPDATA "Android\Sdk"
$bt = Join-Path $sdk "build-tools\36.1.0"
$androidJar = Join-Path $sdk "platforms\android-36\android.jar"
$ndkBin = Join-Path $sdk "ndk\28.2.13676358\toolchains\llvm\prebuilt\windows-x86_64\bin"
$out = Join-Path $root "build\android"
$minSdk = 21
$targetSdk = 35

$jdk = if ($env:JAVA_HOME) { $env:JAVA_HOME } else { (Get-ChildItem "C:\Program Files\Java" -Directory -Filter "jdk-*" | Sort-Object Name | Select-Object -Last 1).FullName }
$env:PATH = "D:\CLAUDE_CODE\tools\go\bin;C:\Program Files\nodejs;$jdk\bin;$env:PATH"
$env:GOPATH = "D:\CLAUDE_CODE\tools\gopath"
$env:GOTOOLCHAIN = "local"

function Step($name) { Write-Host "== $name" }
function Check($what) { if ($LASTEXITCODE -ne 0) { throw "$what завершился с ошибкой ($LASTEXITCODE)" } }

if (Test-Path $out) { Remove-Item -Recurse -Force $out }
New-Item -ItemType Directory -Force "$out\lib", "$out\assets", "$out\gen", "$out\classes", "$out\dex" | Out-Null

# 1. Go core for each ABI
$targets = @{
    "arm64-v8a"   = @{ GOARCH = "arm64"; CC = "aarch64-linux-android$minSdk-clang.cmd" }
    "armeabi-v7a" = @{ GOARCH = "arm"; GOARM = "7"; CC = "armv7a-linux-androideabi$minSdk-clang.cmd" }
    "x86_64"      = @{ GOARCH = "amd64"; CC = "x86_64-linux-android$minSdk-clang.cmd" }
}
Push-Location $root
try {
    foreach ($abi in $Abis) {
        Step "Go core: $abi"
        $t = $targets[$abi]
        $env:CGO_ENABLED = "1"; $env:GOOS = "android"; $env:GOARCH = $t.GOARCH; $env:GOARM = $t.GOARM
        $env:CC = Join-Path $ndkBin $t.CC
        New-Item -ItemType Directory -Force "$out\lib\$abi" | Out-Null
        go build -buildmode=c-shared -trimpath -ldflags "-s -w -buildid= -extldflags=-Wl,-z,max-page-size=16384" -o "$out\lib\$abi\libawgpilot.so" ./mobile
        Check "go build ($abi)"
        Remove-Item "$out\lib\$abi\libawgpilot.h"
    }
} finally {
    Remove-Item Env:CGO_ENABLED, Env:GOOS, Env:GOARCH, Env:GOARM, Env:CC -ErrorAction SilentlyContinue
    Pop-Location
}

# 2. Web UI (older Android WebViews: transpile to ES2017)
Step "web UI"
Push-Location (Join-Path $root "frontend")
try {
    npx tsc; Check "tsc"
    npx vite build --outDir "$out\assets\web" --emptyOutDir --target es2017 --logLevel warn; Check "vite build"
} finally { Pop-Location }

# 3. Resources and manifest
Step "aapt2"
& "$bt\aapt2.exe" compile --dir "$PSScriptRoot\res" -o "$out\res.zip"; Check "aapt2 compile"
& "$bt\aapt2.exe" link -o "$out\base.apk" -I $androidJar --manifest "$PSScriptRoot\AndroidManifest.xml" `
    --min-sdk-version $minSdk --target-sdk-version $targetSdk --version-code $VersionCode --version-name $Version `
    --java "$out\gen" "$out\res.zip"; Check "aapt2 link"

# 4. Java -> dex
Step "javac + d8"
$sources = @(Get-ChildItem -Recurse "$PSScriptRoot\src", "$out\gen" -Filter *.java | ForEach-Object FullName)
javac --release 11 -encoding UTF-8 -nowarn -Xlint:-options -cp $androidJar -d "$out\classes" @sources; Check "javac"
Push-Location "$out\classes"; jar cf "$out\classes.jar" .; Check "jar"; Pop-Location
& "$bt\d8.bat" --release --min-api $minSdk --lib $androidJar --output "$out\dex" "$out\classes.jar"; Check "d8"

# 5. Assemble, align, sign
Step "apk"
Add-Type -AssemblyName System.IO.Compression, System.IO.Compression.FileSystem
Copy-Item "$out\base.apk" "$out\unsigned.apk"
$zip = [System.IO.Compression.ZipFile]::Open("$out\unsigned.apk", "Update")
try {
    [void][System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile($zip, "$out\dex\classes.dex", "classes.dex")
    foreach ($abi in $Abis) {
        [void][System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile($zip, "$out\lib\$abi\libawgpilot.so", "lib/$abi/libawgpilot.so")
    }
    # Assets are added here, not with aapt2 -A, which writes Windows "\" separators into entry names.
    $assets = (Resolve-Path "$out\assets").Path
    foreach ($f in Get-ChildItem -Recurse -File $assets) {
        $name = "assets/" + $f.FullName.Substring($assets.Length + 1).Replace("\", "/")
        [void][System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile($zip, $f.FullName, $name)
    }
} finally { $zip.Dispose() }
& "$bt\zipalign.exe" -p -f 4 "$out\unsigned.apk" "$out\aligned.apk"; Check "zipalign"

$ks = Join-Path $SigningDir "awgpilot-release.jks"
$passFile = Join-Path $SigningDir "keystore-password.txt"
if (-not (Test-Path $ks)) {
    Step "signing key (one time)"
    New-Item -ItemType Directory -Force $SigningDir | Out-Null
    $bytes = New-Object byte[] 24
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    $pass = [Convert]::ToBase64String($bytes) -replace '[+/=]', 'x'
    Set-Content -Path $passFile -Value $pass -NoNewline -Encoding ascii
    keytool -genkeypair -keystore $ks -storetype PKCS12 -alias awgpilot -keyalg RSA -keysize 3072 -validity 10000 `
        -dname "CN=AWG Pilot" -storepass $pass -keypass $pass; Check "keytool"
}
$apk = Join-Path $out "awgpilot-$Version.apk"
& "$bt\apksigner.bat" sign --ks $ks --ks-key-alias awgpilot --ks-pass "file:$passFile" --out $apk "$out\aligned.apk"; Check "apksigner sign"
& "$bt\apksigner.bat" verify $apk; Check "apksigner verify"
Remove-Item "$out\unsigned.apk", "$out\aligned.apk", "$out\base.apk"
Write-Host "Готово: $apk ($([math]::Round((Get-Item $apk).Length / 1MB, 1)) МБ)"
