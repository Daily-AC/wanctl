在**要被控制的那台机器**上跑三行——装工具、配置实例地址（一次即可）、登录接入：

```
curl -fsSL https://github.com/Daily-AC/wanctl/releases/latest/download/install.sh | sh
wanctl config set relay=https://relay.example.com portal=https://portal.example.com
wanctl start
```

浏览器会弹门户登录（GitHub 账号）并显示一个一次性 code，贴回终端回车即可。之后服务转入后台，`wanctl stop` 停、`wanctl status` 看状态。装的时候**不需要任何 token**。

> 第一次能登录门户的前提是你已经在这个部署里：部署的第一个登录用户自动成为管理员，之后的人要由管理员放行：按 GitHub 用户名邀请，或者登录后在申请页提交申请、等管理员通过（见「邀请、好友与共享」）。
>
> 跳过第二行直接跑 `wanctl start` 也行——第一次会在终端里问你用哪个中继：这套部署自己的，还是项目的官方实例，答完就存下来。配好的地址用 `wanctl config` 随时查看或修改，环境变量 `WANCTL_RELAY`/`WANCTL_PORTAL` 仍可临时覆盖。这个提问不会挡住任何脚本：没有终端、或者设了 `WANCTL_NO_PROMPT=1`，命令就直接打印那行 `wanctl config set` 然后退出。

工具和安装脚本都来自项目的 [GitHub Releases](https://github.com/Daily-AC/wanctl/releases)：安装器先验证签名过的发布清单，再核对二进制的大小和哈希，然后才落盘（用系统自带的 `openssl`，macOS 的 LibreSSL 也可以）——发布源与你的中继相互独立，中继出问题也装得上。

## 装成服务，重启后还在

`wanctl start` 起的 agent 关掉终端还在，但注销或重启之后就没了。要长期被控的机器，登录过一次之后改成系统服务（先停掉 `wanctl start` 起的那个，免得两个抢同一个配置目录）：

```
wanctl stop
wanctl service install
wanctl service status
```

`service install` 在 macOS 上装 launchd 代理，在 Linux 上装 systemd 用户服务，在 Windows 上装计划任务，agent 意外退出会被自动拉起。配好的 relay 地址和传输方式会写进服务单元，重启后不依赖环境变量（`--relay` / `--transport` 可显式指定）。macOS 和 Windows 上它在用户登录系统之后才启动，没人值守的机器要开自动登录；Linux 上它会顺手尝试 `loginctl enable-linger`，成功了开机不用登录也能起来，失败时会提示你用 sudo 再跑一次。以后要停用它，跑 `wanctl service uninstall`。

Windows 机器看[下一篇](#docs/windows-install)。
