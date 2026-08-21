在**要被控制的那台机器**上跑三行——装工具、配置实例地址（一次即可）、登录接入：

```
curl -fsSL https://github.com/Daily-AC/wanctl/releases/latest/download/install.sh | sh
wanctl config set relay=https://relay.example.com portal=https://portal.example.com
wanctl
```

浏览器会弹门户登录（GitHub 账号）并显示一个一次性 code，贴回终端回车即可。之后服务转入后台，`wanctl stop` 停、`wanctl status` 看状态。装的时候**不需要任何 token**。

> 第一次能登录门户的前提是你已经在这个部署里：部署的第一个登录用户自动成为管理员，之后的人要拿管理员发的邀请码，登录后兑换（见「邀请、好友与共享」）。
>
> 跳过第二行直接跑 `wanctl` 也行——第一次会在终端里引导你填这两个地址。配好的地址用 `wanctl config` 随时查看或修改，环境变量 `WANCTL_RELAY`/`WANCTL_PORTAL` 仍可临时覆盖。

工具和安装脚本都来自项目的 [GitHub Releases](https://github.com/Daily-AC/wanctl/releases)：安装器先验证签名过的发布清单，再核对二进制的大小和哈希，然后才落盘（用系统自带的 `openssl`，macOS 的 LibreSSL 也可以）——发布源与你的中继相互独立，中继出问题也装得上。

想要开机自启，跑 `wanctl service install`——它会把配置好的 relay 地址和传输方式写进服务单元，重启后不依赖环境变量（`--relay` / `--transport` 可显式指定）。

Windows 机器看[下一篇](#docs/windows-install)。
