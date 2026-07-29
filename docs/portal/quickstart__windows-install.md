Windows 上装 wanctl 只要一行。**注意开头那半句 openssl 不能省**——安装器启动时就检查它，缺了会直接报错退出，而公司发的 Windows 机器上基本都没有。

```powershell
winget install ShiningLight.OpenSSL.Light; $env:PATH += ';C:\Program Files\OpenSSL-Win64\bin'; $env:WANCTL_RELAY='https://wanctl-relay.***REMOVED***.***REMOVED***.com'; irm "$env:WANCTL_RELAY/install.ps1" | iex
```

装完把这台机器接进你的空间：

```powershell
$env:WANCTL_TRANSPORT='http'
wanctl agent --relay https://wanctl-relay.***REMOVED***.***REMOVED***.com --token <你的 token> --transport http --name $env:COMPUTERNAME
wanctl service install
```

`wanctl service install` 会注册一个开机自启的计划任务，关掉终端窗口它也继续跑。之后 `wanctl status` 看状态、`wanctl stop` 停。

token 在门户「设备 / 签发 token」里拿，明文只显示一次。

## 如果这台机器已经不干净

`irm | iex` 是把远端代码直接执行，脚本不落盘，也就没法在运行前核对它。给一台**已经中毒或来路可疑**的机器装工具时，改用两步式：

```powershell
irm "$env:WANCTL_RELAY/install.ps1" -OutFile install.ps1
(Get-FileHash -Algorithm SHA256 .\install.ps1).Hash.ToLower()
.\install.ps1
```

关键在于：**核对用的哈希必须从别的渠道拿**——飞书上问管理员要，或者从 GitLab release 页面读。从 relay 上下载脚本、又从 relay 上读哈希来验证它，等于自己证明自己，白比。

能访问公司 GitLab 的同事，最稳的做法是直接从 [release 页面](https://g.***REMOVED***.com/***REMOVED***/wanctl-releases/-/releases/v0.1.0) 下载安装器——那是独立于 relay 的信任起点。relay 分发这条路是给没有 GitLab 账号的同事准备的。

## Windows 上的两个已知坑

**输出里每个字之间夹空格**：Windows 原生工具（尤其 `wsl.exe`）吐的是 UTF-16LE，被按 OEM 代码页解码后零字节残留成了分隔符。新版已经在会话开头注入 `WSL_UTF8=1` 和 UTF-8 编码设置处理掉了，如果你还看得到，说明 agent 是旧的，跑 `wanctl update`。

**引号被吃掉**：Windows 的登录 shell 是 PowerShell，`$`、引号在多层转义里很容易被吞。命令复杂时别硬拼，用 `wanctl push` 把脚本送过去再执行，或者走 `-EncodedCommand`（base64 UTF-16LE）。
