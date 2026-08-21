在 PowerShell 里跑三行——装工具、配置实例地址（一次即可）、登录接入：

```powershell
irm https://github.com/Daily-AC/wanctl/releases/latest/download/install.ps1 | iex
wanctl config set relay=https://relay.example.com portal=https://portal.example.com
wanctl
```

不需要装 OpenSSL——安装器用 PowerShell 自带的加密接口验证发布签名，验过了才落盘。Windows 7 之后的每个版本都自带够用的 PowerShell。

登录时浏览器会弹门户登录（GitHub 账号）并给一个一次性 code，贴回终端回车。之后：

```powershell
wanctl service install
```

这会注册一个开机自启的计划任务，关掉终端窗口它也继续跑（relay 地址会直接写进计划任务，重启后不依赖环境变量）。`wanctl status` 看状态、`wanctl stop` 停。

## 关于信任

脚本和二进制都来自项目的 [GitHub Releases](https://github.com/Daily-AC/wanctl/releases)，与你的中继相互独立；脚本落盘前会验证发布签名、大小和 SHA-256。内网自建镜像的场景，给安装命令前面设置 `$env:WANCTL_RELAY` 即可改从中继的 /dl 拉取。

## Windows 上的两个已知坑

**输出里每个字之间夹空格**：Windows 原生工具（尤其 `wsl.exe`）吐的是 UTF-16LE，被按 OEM 代码页解码后零字节残留成了分隔符。新版已经处理掉了，如果你还看得到，说明 agent 是旧的，跑 `wanctl update`。

**引号被吃掉**：Windows 的登录 shell 是 PowerShell，`$`、引号在多层转义里很容易被吞。命令复杂时别硬拼，用 `wanctl push` 把脚本送过去再执行。
