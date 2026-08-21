在**要被控制的那台机器**上，先把两个地址放进环境（写进 `~/.zshrc` / `~/.bashrc` 一劳永逸）：

```
export WANCTL_RELAY=https://relay.example.com
export WANCTL_PORTAL=https://portal.example.com
```

然后两行——第一行装好工具，第二行登录把它绑到你的空间：

```
curl -fsSL $WANCTL_RELAY/install.sh | sh
wanctl
```

浏览器会弹门户登录（GitHub 账号）并显示一个一次性 code，贴回终端回车即可。之后服务转入后台，`wanctl stop` 停、`wanctl status` 看状态。装的时候**不需要任何 token**。

> 第一次能登录门户的前提是你已经在这个部署里：部署的第一个登录用户自动成为管理员，之后的人要拿管理员发的邀请码，登录后兑换（见「邀请、好友与共享」）。
>
> 注：第一次跑 `wanctl` 会同时完成「授权 → 后台启动」两件事；之后 `wanctl start/stop/status` 管 daemon。

安装器会先验证一份签名过的发布清单，再核对二进制的大小和哈希，然后才落盘。用的是系统自带的 `openssl`（macOS 的 LibreSSL 也可以），不需要额外装什么。

想要开机自启，跑 `wanctl service install`——它会把当时生效的 relay 地址和传输方式一起写进服务单元，重启后不依赖任何环境变量（也可用 `--relay` / `--transport` 显式指定）。

Windows 机器看[下一篇](#docs/windows-install)。
