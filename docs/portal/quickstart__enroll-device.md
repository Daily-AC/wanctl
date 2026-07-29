在**要被控制的那台机器**上跑下面两行。第一行装好工具，第二行用飞书登录把它绑到你的空间：

```
curl -fsSL https://wanctl-relay.***REMOVED***.***REMOVED***.com/install.sh | sh
wanctl
```

浏览器会弹飞书登录并显示一个一次性 code，贴回终端回车即可。之后服务转入后台，`wanctl stop` 停、`wanctl status` 看状态。装的时候**不需要任何 token**。

> 注：第一次跑 `wanctl` 会同时完成「授权 → 后台启动」两件事；之后 `wanctl start/stop/status` 管 daemon。

安装器会先验证一份签名过的发布清单，再核对二进制的大小和哈希，然后才落盘。用的是系统自带的 `openssl`（macOS 的 LibreSSL 也可以），不需要额外装什么。

Windows 机器看[下一篇](#docs/windows-install)。
