wanctl 的每条命令都经 relay 中转，而且要来回好几趟：先建连接，再和设备做端到端的 TLS 握手，然后才执行命令。你的机器到 relay 这段路上多出来的每一点延迟，一条命令都要付好几遍。所以用得顺不顺，主要看这段路。

## 用了代理，就让 relay 走直连

代理软件开着 TUN 模式时，发往 relay 的流量会按代理规则分流，很容易被分到很远的节点上。一次实测（2026-09-25，北京）：一台开着 Clash Verge TUN 的 Mac，去香港服务器的流量被分到了洛杉矶节点，首字节时间中位 1.25 秒、最慢 3.9 秒；同一时刻从家里直连只要 0.11 秒。

先查你连的是哪个 relay，输出里 `relay` 那一行就是（`wanctl status` 里也有）：

```
wanctl config
```

然后给这个域名加一条直连规则，放在所有规则的最前面，免得被订阅里更靠前的规则抢先命中。直接写 mihomo / Clash 配置的话是这样：

```yaml
rules:
  - DOMAIN,relay.example.com,DIRECT
  # ...your existing rules
```

用 Clash Verge Rev 的话别直接改订阅文件，订阅一更新就会被整个覆盖；在订阅的菜单里点「编辑规则」，用「添加前置规则」加这一条。加完到「连接」页找到 relay 的域名，确认它走的是 DIRECT。其他代理软件同理。

## wanctl 只认环境变量里的代理

wanctl 不读系统代理设置：macOS 网络设置里的代理、Windows 的 Internet 选项、代理软件的「系统代理」开关，对它都不起作用。它只读 `HTTPS_PROXY`、`HTTP_PROXY`、`NO_PROXY` 这几个环境变量（大小写都行）。分三种情况：

- 代理软件只开了「系统代理」，没开 TUN，终端里也没设代理变量：wanctl 本来就是直连，不用做什么。
- 开了 TUN：按上一节加直连规则。
- 终端里设了 `HTTPS_PROXY`：wanctl 会走这个代理，而且不会用 HTTP/3。

第三种情况推荐把 relay 加进 `NO_PROXY`，让 wanctl 直连。wanctl 在 UDP 走得通的网络上会自动换用 HTTP/3，在给 TCP 限速的网络上它快得多，走了代理就用不上：

```
export NO_PROXY="$NO_PROXY,relay.example.com"
```

注意，用 `wanctl service install` 装成服务的 agent 读不到你在终端或 shell 配置文件里设的变量；用 `wanctl start` 从终端启动的 agent 会继承那个终端的变量。

反过来，如果你的网络放行 UDP、但转得比 TCP 还差（部分 TUN 模式代理就是这样），设 `WANCTL_HTTP3=0` 让 wanctl 只走 HTTP/2，见[环境变量参考](https://wc.z10.dev/docs/environment/)。

## 大文件：1 GiB 以内用 push，再大的换工具

`wanctl push` 单个文件最大 1 GiB，超过了会在开始传之前报错：

```
wanctl: upload size 1073741825 outside supported range 0..1073741824
```

`pull` 没有这个上限，但两者都整条经 relay 中转，速度取决于两端各自到 relay 的网络，中途断了也只能从头再传。所以：

- 目录或一堆小文件，先打成一个压缩包再传。`push` 和 `pull` 一次只传一个文件，每个文件都要和设备重新建一次连接。
- 超过 1 GiB 的，或者很大还要反复传的，两台机器能直接 SSH 就用 `scp` 或 `rsync`（`rsync` 能续传）；连不上就走网盘、对象存储这类中转。

## 被控的机器装成服务，别只跑 wanctl start

`wanctl start` 把 agent 放到后台，关掉终端它还在，但注销或重启之后就没了；`wanctl login` 只保存凭证，不启动 agent。只跑过这两条的机器，重启一次就离线。要长期被控的机器，登录过一次之后改成系统服务（先停掉 `wanctl start` 起的那个，免得两个抢同一个配置目录）：

```
wanctl stop
wanctl service install
wanctl service status
```

`service install` 在 macOS 上装 launchd 代理，在 Linux 上装 systemd 用户服务，在 Windows 上装计划任务，agent 意外退出会被自动拉起。macOS 和 Windows 上它在用户登录系统之后才启动，没人值守的机器要开自动登录；Linux 上它会顺手尝试 `loginctl enable-linger`，成功了开机不用登录也能起来，失败时会提示你用 sudo 再跑一次。以后要停用它，跑 `wanctl service uninstall`。

## 慢的时候怎么自查

按下面的顺序查，每一步排除一种原因。

**先看版本。** 本机跑 `wanctl version`，对端跑 `wanctl status --target DEVICE`，它会报对端的 agent 版本。HTTP/3 从 v0.13.0 起才有。agent 默认会自己更新，控制端用 `wanctl update` 升级。

**再看流量有没有绕路。** `env | grep -i proxy` 看终端里有没有代理变量；开着 TUN 的话，到代理软件的连接列表里找 relay 的域名，看它是不是 DIRECT。不是的话，回到前两节。

**然后量这段路本身。** 前两步都处理好之后，连跑几次下面这条，它打印的是到 relay 的首字节时间（秒）：

```
curl --noproxy '*' -so /dev/null -w '%{time_starttransfer}\n' https://relay.example.com/healthz
```

作为参照，上面那次实测里直连是 0.11 秒。已经直连、这个数还是很大，就是你的网络到 relay 这段本身慢，wanctl 这边没什么可调的，大文件按上文换工具。

**最后看 HTTP/3 帮没帮上忙。** wanctl 不显示它在用哪个协议，但可以在控制端对比：同一个几十 MB 的文件，正常 push 和关掉 HTTP/3 再 push 各跑几次，网速有波动，只跑一次不作数。

```
time wanctl push --target DEVICE ./test.bin /tmp/test.bin
time WANCTL_HTTP3=0 wanctl push --target DEVICE ./test.bin /tmp/test.bin
```

关掉之后明显更快，说明你的网络放行 UDP 但转得差，以后带着 `WANCTL_HTTP3=0` 用；两次差不多，说明 HTTP/3 没用上（比如 UDP 被挡，wanctl 已经自动退回 HTTP/2），或者瓶颈不在这里。
