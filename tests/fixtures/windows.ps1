$ErrorActionPreference = "Stop"
$root = "C:\ProgramData\NetlabTests"
New-Item -ItemType Directory -Path $root -Force | Out-Null
Start-Transcript -Path "$root\setup.log"
try {
    foreach ($msi in @("qemu-ga-x86_64.msi", "CloudbaseInitSetup_1_1_8_x64.msi")) {
        $result = Start-Process msiexec.exe -ArgumentList @("/i", "`"$PSScriptRoot\$msi`"", "/qn", "/norestart") -Wait -PassThru
        if ($result.ExitCode -notin @(0, 3010)) { throw "$msi installation failed: $($result.ExitCode)" }
    }
    Stop-Service cloudbase-init
    $config = "C:\Program Files\Cloudbase Solutions\Cloudbase-Init\conf\cloudbase-init.conf"
    @'
[DEFAULT]
username=netlab
metadata_services=cloudbaseinit.metadata.services.configdrive.ConfigDriveService
plugins=cloudbaseinit.plugins.common.sethostname.SetHostNamePlugin,cloudbaseinit.plugins.common.networkconfig.NetworkConfigPlugin,cloudbaseinit.plugins.common.sshpublickeys.SetUserSSHPublicKeysPlugin
allow_reboot=true
stop_service_on_exit=true
logdir=C:\Program Files\Cloudbase Solutions\Cloudbase-Init\log
logfile=cloudbase-init.log
'@ | Set-Content -LiteralPath $config -Encoding ASCII
    Set-Service cloudbase-init -StartupType Automatic
    Set-Service qemu-ga -StartupType Automatic
    Start-Service qemu-ga
    Set-LocalUser -Name netlab -PasswordNeverExpires $true
    New-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Control\TimeZoneInformation" -Name RealTimeIsUniversal -Value 1 -PropertyType DWord -Force | Out-Null
    Set-ItemProperty "HKLM:\SYSTEM\CurrentControlSet\Control\Terminal Server" fDenyTSConnections 0
    New-NetFirewallRule -Name NetlabTestRDP -DisplayName NetlabTestRDP -Direction Inbound -Action Allow -Protocol TCP -LocalPort 3389 | Out-Null
    if (!(Test-Path "C:\Windows\System32\OpenSSH\sshd.exe")) { throw "Windows Server 2025 OpenSSH Server is missing" }
    New-Item -ItemType Directory -Path "C:\ProgramData\ssh" -Force | Out-Null
    @'
Port 22
PubkeyAuthentication yes
PasswordAuthentication yes
Subsystem sftp sftp-server.exe
'@ | Set-Content -LiteralPath "C:\ProgramData\ssh\sshd_config" -Encoding ASCII
    New-Item -Path "HKLM:\SOFTWARE\OpenSSH" -Force | Out-Null
    New-ItemProperty -Path "HKLM:\SOFTWARE\OpenSSH" -Name DefaultShell -Value "C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe" -PropertyType String -Force | Out-Null
    Set-Service sshd -StartupType Automatic
    New-NetFirewallRule -Name NetlabTestSSH -DisplayName NetlabTestSSH -Direction Inbound -Action Allow -Protocol TCP -LocalPort 22 | Out-Null
    Set-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon" DefaultPassword ""
    Set-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon" AutoAdminLogon 0
    @{version=(Get-CimInstance Win32_OperatingSystem).Caption; secureBoot=(Confirm-SecureBootUEFI); tpm=(Get-Tpm).TpmPresent} | ConvertTo-Json | Set-Content "$root\setup.done"
    Start-Service sshd
} catch {
    Write-Output $_
    throw
} finally {
    Stop-Transcript
}
