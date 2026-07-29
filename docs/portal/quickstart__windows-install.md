在 PowerShell 里跑这一行：

```powershell
irm https://wanctl-relay.***REMOVED***.***REMOVED***.com/install.ps1 | iex
```

不需要装 OpenSSL，也不需要设任何环境变量——安装器用 PowerShell 自带的加密接口验证发布签名，验过了才落盘。Windows 7 之后的每个版本都自带够用的 PowerShell。

装完把这台机器接进你的空间：

```powershell
wanctl
```

浏览器会弹飞书登录并给一个一次性 code，贴回终端回车。之后：

```powershell
wanctl service install
```

这会注册一个开机自启的计划任务，关掉终端窗口它也继续跑。`wanctl status` 看状态、`wanctl stop` 停。

## 装到重要的机器上

`irm | iex` 是把远端代码直接执行。脚本会验证它下载的二进制，但**验证脚本本身来自同一个地方**——真出了事，能换二进制的人通常也能换这个脚本。给一台你特别在意的机器装的时候，从 [GitLab release 页面](https://g.***REMOVED***.com/ai-native/wanctl/-/releases) 下载 `install.ps1` 再运行那个文件，那是独立于 relay 的信任起点。

relay 直接分发这条路是给没有 GitLab 账号的同事准备的。

## Windows 上的两个已知坑

**输出里每个字之间夹空格**：Windows 原生工具（尤其 `wsl.exe`）吐的是 UTF-16LE，被按 OEM 代码页解码后零字节残留成了分隔符。新版已经处理掉了，如果你还看得到，说明 agent 是旧的，跑 `wanctl update`。

**引号被吃掉**：Windows 的登录 shell 是 PowerShell，`$`、引号在多层转义里很容易被吞。命令复杂时别硬拼，用 `wanctl push` 把脚本送过去再执行。
