在 PowerShell 里跑三行——装工具、配置实例地址（一次即可）、登录接入：

```powershell
irm https://github.com/Daily-AC/wanctl/releases/latest/download/install.ps1 | iex
wanctl config set relay=https://relay.example.com portal=https://portal.example.com
wanctl start
```

不需要装 OpenSSL——安装器用 PowerShell 自带的加密接口验证发布签名，验过了才落盘。Windows 7 之后的每个版本都自带够用的 PowerShell。

登录时浏览器会弹门户登录（GitHub 账号）并给一个一次性 code，贴回终端回车。之后：

```powershell
wanctl service install
```

这会注册一个登录时自启的计划任务，以你自己的身份运行，关掉终端窗口它也继续跑（relay 地址会直接写进计划任务，重启后不依赖环境变量）。`wanctl status` 看状态、`wanctl stop` 停。

## 关于信任

脚本和二进制都来自项目的 [GitHub Releases](https://github.com/Daily-AC/wanctl/releases)，与你的中继相互独立；脚本落盘前会验证发布签名、大小和 SHA-256。内网自建镜像的场景，给安装命令前面设置 `$env:WANCTL_RELAY` 即可改从中继的 /dl 拉取。

## Windows 上的两个已知坑

**输出里每个字之间夹空格**：Windows 原生工具（尤其 `wsl.exe`）吐的是 UTF-16LE，被按 OEM 代码页解码后零字节残留成了分隔符。新版已经处理掉了，如果你还看得到，说明 agent 是旧的，跑 `wanctl update`。

**引号被吃掉**：Windows 的登录 shell 是 PowerShell，`$`、引号在多层转义里很容易被吞。命令复杂时别硬拼，用 `wanctl push` 把脚本送过去再执行。

## agent 以 SYSTEM 身份运行时，按登录用户身份跑命令

如果你把 agent 装成了 SYSTEM 身份的服务（`wanctl service install` 不会这样装），命令也以 SYSTEM 身份执行。per-user 的安装器这时会装错地方，或者在用户目录里留下 SYSTEM 拥有的文件，用户自己以后改不动、删不掉。

办法是借一次性的计划任务，以已登录用户的身份跑。把下面这段存成 `run-as-user.ps1`，改掉 `$User` 和两行 `'@` 之间的命令，然后发过去：`wanctl exec --target DEVICE --script run-as-user.ps1`。命令在那个用户的桌面会话里、以他平时的权限（不提权）运行，环境变量、HKCU 和新建文件的属主都是他的。输出和退出码会原样带回来；超过 `$TimeoutSec`（默认 10 分钟）就连同子进程一起结束；跑完会删掉任务和临时文件。

```powershell
# run-as-user.ps1: run the block below as a signed-in user, from an agent running as SYSTEM.
$User = 'alice'          # `quser` lists who is signed in; MACHINE\alice works too
$TimeoutSec = 600
$Payload = @'
whoami
$env:LOCALAPPDATA
'@

$ErrorActionPreference = 'Stop'
$sid = (New-Object Security.Principal.NTAccount $User).Translate([Security.Principal.SecurityIdentifier]).Value
$User = (New-Object Security.Principal.SecurityIdentifier $sid).Translate([Security.Principal.NTAccount]).Value
if (-not (Get-Process explorer -IncludeUserName | Where-Object UserName -eq $User)) {
    [Console]::Error.WriteLine("$User is not signed in; the task would never start")
    exit 1
}
$home_ = (Get-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList\$sid").ProfileImagePath
# Stage in the user's own Temp: a script SYSTEM leaves in C:\Windows\Temp is unreadable to the user.
$id = 'wanctl-as-' + [guid]::NewGuid().ToString('N').Substring(0, 8)
$dir = Join-Path $home_ "AppData\Local\Temp\$id"
New-Item -ItemType Directory $dir | Out-Null
"`$PID | Set-Content `"$dir\pid.txt`"; [Console]::OutputEncoding = [Text.Encoding]::UTF8`r`n" + $Payload | Set-Content "$dir\payload.ps1" -Encoding UTF8
@"
@powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0payload.ps1" > "%~dp0out.txt" 2>&1
@(echo %ERRORLEVEL%)> "%~dp0rc.txt"
"@ | Set-Content "$dir\run.cmd" -Encoding ASCII

# Interactive logon type: runs in the user's desktop session with their own token, no password needed.
# conhost --headless keeps a console window from popping up on that desktop.
$action = New-ScheduledTaskAction -Execute 'conhost.exe' -Argument "--headless cmd.exe /c `"$dir\run.cmd`"" -WorkingDirectory $home_
$principal = New-ScheduledTaskPrincipal -UserId $User -LogonType Interactive -RunLevel Limited
# Task defaults refuse to start on battery, which would leave a laptop waiting for the timeout.
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
Register-ScheduledTask -TaskName $id -Action $action -Principal $principal -Settings $settings | Out-Null
try {
    Start-ScheduledTask -TaskName $id
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while (-not (Test-Path "$dir\rc.txt") -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 300 }
    if (-not (Test-Path "$dir\rc.txt")) {
        # Stopping the task only ends conhost; take down the payload's own process tree.
        if (Test-Path "$dir\pid.txt") { taskkill /PID (Get-Content "$dir\pid.txt") /T /F | Out-Null }
        Stop-ScheduledTask -TaskName $id
        Start-Sleep 1
        [Console]::Error.WriteLine("timed out after $TimeoutSec s (is $User signed in?)")
        $rc = 124
    } else {
        Get-Content "$dir\out.txt" -Encoding UTF8
        $rc = [int](Get-Content "$dir\rc.txt")
    }
} finally {
    Unregister-ScheduledTask -TaskName $id -Confirm:$false
    Remove-Item $dir -Recurse -Force -ErrorAction SilentlyContinue
}
exit $rc
```

三点限制：

- 输出等命令跑完才一次性回来，不是边跑边出。
- 目标用户得已经登录，没登录会直接报错。
- 中途按 Ctrl-C 只停得掉外层脚本，用户会话里的命令会继续跑完，任务和临时目录会留下来。可以用下面两行清理：

```powershell
Get-ScheduledTask wanctl-as-* | Unregister-ScheduledTask -Confirm:$false
Remove-Item "C:\Users\*\AppData\Local\Temp\wanctl-as-*" -Recurse -Force
```
