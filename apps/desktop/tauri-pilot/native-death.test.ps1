$ErrorActionPreference = 'Stop'
$exe = (Resolve-Path (Join-Path $PSScriptRoot 'target/debug/persona-tauri-pilot.exe')).Path
$app = [System.Diagnostics.Process]::new()
$app.StartInfo.FileName = $exe
$app.StartInfo.Arguments = '--native-death-test'
$app.StartInfo.WorkingDirectory = $PSScriptRoot
$app.StartInfo.UseShellExecute = $false
$app.StartInfo.RedirectStandardOutput = $true
$app.StartInfo.RedirectStandardError = $true
try {
    [void]$app.Start()
    $errors = $app.StandardError.ReadToEndAsync()
    $ready = $app.StandardOutput.ReadLineAsync()
    if (-not $ready.Wait(30000) -or $ready.Result -ne 'pilot: native cookie verified; opening temporary loopback UI') {
        throw 'Native cookie verification did not complete'
    }
    $supervisor = @(Get-CimInstance Win32_Process -Filter "ParentProcessId=$($app.Id)" | Where-Object {
        $_.Name -eq 'node.exe' -and $_.CommandLine -like '*tauri-pilot*supervisor.mjs*'
    })
    if ($supervisor.Count -ne 1) { throw 'Expected exactly one owned supervisor' }
    $service = @(Get-CimInstance Win32_Process -Filter "ParentProcessId=$($supervisor[0].ProcessId)" | Where-Object {
        $_.Name -eq 'node.exe' -and $_.CommandLine -like '*tauri-pilot*service-child.mjs*'
    })
    if ($service.Count -ne 1) { throw 'Expected exactly one owned service' }
    $owner = Invoke-CimMethod -InputObject $service[0] -MethodName GetOwner
    if ($owner.User -ne [Environment]::UserName -or $service[0].ExecutablePath -ne $supervisor[0].ExecutablePath) {
        throw 'Service path/owner mismatch'
    }
    # Terminate only the confirmed child created for this disposable pilot run.
    Stop-Process -Id $service[0].ProcessId
    if (-not $app.WaitForExit(30000)) { throw 'Tauri did not exit after owned service death' }
    if ($app.ExitCode -ne 1) { throw "Expected terminal failure exit 1, got $($app.ExitCode)" }
    if (Get-Process -Id $supervisor[0].ProcessId -ErrorAction SilentlyContinue) { throw 'Supervisor still running' }
    Write-Output 'native-cookie=verified owned-service-death=terminal tauri-exit=1 supervisor=exited reconnect=none'
} finally {
    if ($app.Id -and -not $app.HasExited) {
        [void]$app.CloseMainWindow()
        if (-not $app.WaitForExit(45000)) { throw 'Pilot cleanup did not settle; inspect its identified PID' }
    }
    $app.Dispose()
}
