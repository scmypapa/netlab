$ErrorActionPreference = 'Stop'
$root = 'C:\ProgramData\NetlabTests'
New-Item -ItemType Directory -Path $root -Force | Out-Null
Start-Transcript -Path "$root\matrix-setup.log"
try {
    $result = Start-Process msiexec.exe -ArgumentList @('/i', "`"$PSScriptRoot\qemu-ga-x86_64.msi`"", '/qn', '/norestart') -Wait -PassThru
    if ($result.ExitCode -notin @(0, 3010)) { throw "Guest Agent installation failed: $($result.ExitCode)" }
    Set-Service qemu-ga -StartupType Automatic
    Start-Service qemu-ga
    Set-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon' DefaultPassword ''
    Set-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon' AutoAdminLogon 0
    Set-Content "$root\matrix-setup.done" 'installed'
} finally {
    Stop-Transcript
}
