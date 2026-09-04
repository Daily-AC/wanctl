# wanctl — 架构

从一个终端（或者一个 AI agent 的 shell）跨公网控制另一台设备，走的是一条端到端加密、
由 relay 转发的通道。网页门户签发令牌、管理共享；由 Postgres 支撑的 relay 负责认证和
撮合；设备本地执行一套基于审批的权限策略。

```
controller (you/agent) ──┐                            ┌── device (wanctl agent)
   wanctl exec/push/logs  │   relay (public broker)   │     policy engine + approval
                          ├── byte-pipe + registry ───┤     JSONL event log
                          │   token auth + ACL + audit│
   E2E mutual-TLS ════════╪════════ over the pipe ════╪════ (relay sees only ciphertext)
                          │                           │
   portal (web, SSO) ─────┘  issues tokens, ACL ──────┘  (thin proxy to relay /admin/*)
```

## 组成

一个 Go 二进制，角色在运行时决定：

- `wanctl relay` —— 公网撮合方。由 Postgres 支撑（`DATABASE_URL`）：令牌哈希解析、
  跨命名空间 ACL、元数据审计、设备注册表。同时提供由签名清单约束的
  `/dl/<artifact>` 分发，以及给 AI 控制端读的 `/skills` 文档。没有接数据库时退回到
  静态的 `WANCTL_TOKENS` 环境变量映射。受 secret 保护的管理接口 `/admin/*` 是门户的后端。
- `wanctl portal` —— 网页应用。**它自己没有数据库**：它是 relay `/admin/*` 的一层薄代理，
  用共享的 `WANCTL_ADMIN_SECRET` 通信，作用域收在已认证身份所解析出的那个命名空间里。
  门户同时也是一等的控制端：它持有自己的 Ed25519 身份和一个特权拨号令牌，
  通过同一条端到端隧道驱动实时设备控制台。
- `wanctl agent` —— 被控设备。只向 relay 发起出站连接；每个会话先做一次服务端 TLS 握手，
  再做 TOFU 授权，然后才是受策略约束的命令执行与文件服务。它写一份 JSONL 事件日志。
- `wanctl exec|push|pull|peers|id|trust|rules|logs|net|docs|update` —— 控制端命令。

包结构：`transport`（Ed25519 身份、双向 TLS、TOFU）、`protocol`（分帧的线上格式）、
`wsconn`（WebSocket↔net.Conn）、`httpconn`（HTTP 长轮询↔net.Conn，那个与代理无关的载体）、
`relay`（撮合 + http 传输 + pgstore + admin + dist）、`agent`、`client`、
`server`（shell + 文件）、`policy`（规则 + 审批者）、`console`（与传输无关的审批队列）、
`portal`、`eventlog`、`sessionauth`（relay 签发的能力授予）。

## 传输层

两种载体说的是**同一套** TLS + 分帧协议：

- **WebSocket**（`wsconn`）—— 给那些入口保留 WebSocket 升级的部署（直接暴露，
  或者反向代理配好了升级）。
- **HTTP 长轮询**（`httpconn`）—— 与代理无关的默认项：`WANCTL_TRANSPORT=http`。
  上行是每次写一个 POST；下行是一个长轮询 GET，返回可读的字节 / 204 重试 / 410 EOF。
  每个响应都是有限的，这一点让它能穿过那些既剥掉 `Connection: Upgrade`
  **又**缓冲流式响应的边缘（普通的流式 GET 在这种边缘后面会直接死锁——这是实测出来的，
  不是推理出来的）。

在 8 MB 负载上实测，WebSocket 的 push 吞吐是 4~5 倍、pull 约 2 倍；
交互式 exec 的时延和逐行流式输出的间隔在长轮询上是等价的。
传文件为主的机群选 WebSocket，要最大入口兼容性就选长轮询。

## 信任模型

三层彼此独立，攻破一层不会让其余两层跟着塌：

1. **relay 准入** —— 一个 bearer 令牌（`wanctl_<40 hex>`）映射到一个命名空间。
   令牌以 SHA-256 哈希存储，可以带标签和过期时间，随时可吊销。relay 按命名空间和 ACL
   授权*拨号*；它看不见会话内部。
2. **端到端身份** —— 控制端和设备各持一张自签的 Ed25519 证书；会话在被转发的管道*之上*
   跑双向 TLS 1.3，两端按指纹互相钉住（都是 TOFU）。relay 或数据库被攻破只泄漏元数据。
   设备还会拒绝那些不用标签表明自己是谁的控制端发来的配对请求。
3. **设备本地策略** —— 一个由人工审批把关的规则引擎（Claude Code 那种形态：
   允许 / 这个目录一直允许 / 所有目录一直允许 / 拒绝）。命令规则只匹配单条简单命令——
   凡是带 shell 操作符、替换或重定向的，都要求精确匹配。文件规则把实际的打开操作
   绑定到一个目录根上（软链接逃不出去）。`bypass` 模式自动放行但照样记录；
   提权执行是被**刻意**排除在 bypass 和普通 exec 规则之外的。
   无人值守、没有任何审批者订阅时的结果是拒绝，不是挂起。

跨命名空间共享是 relay 侧的一条 ACL 授予 `(owner, device, grantee, perms)`。
这些授予在协议层就是能力受限的：一个被共享的对象最多只能拿到 exec/read/write，
控制台和日志能力在授予里根本表达不出来。relay 通过已认证的 agent 控制通道给每个会话
盖上它的能力，设备在每次请求时再执行一遍。共享设备在门户里是只读的：
审批、规则、模式和解除绑定都还归设备主人。

## 远程设备控制台

门户通过端到端隧道给设备开一条实时控制台：待审批、规则、模式和活动日志，全部实时推送
（`KindApprovalNotif` 带的是完整的控制台状态）。本地 TTY 上的审批决定和门户上的审批决定
汇进同一个队列，先答的那个算数。设备通过一个显式的 `portal_admins` 集合预先信任门户身份，
这个集合在接入时播种，用 `wanctl portal-admins add|list|remove` 管理
（要删掉最后一个会被拒绝）。轮换门户身份是一次有重叠期的迁移：
先把新旧一起播种，部署，核验，然后再删掉旧的。

因为平台边缘可能缓冲流，门户面向浏览器的事件通道用的是 25 秒长轮询，而不是 SSE。

## 服务端日志

relay 和门户把进程日志同时写进一个有界的内存环（2,000 行 / 2 MiB），并通过受 secret
保护的 `GET /admin/logs` 暴露出来。门户提供自己那份环，同时代理 relay 的那份，
所以它是唯一的入口：

```bash
WANCTL_ADMIN_SECRET=... wanctl logs --service portal
WANCTL_ADMIN_SECRET=... wanctl logs --service relay --since 30m --grep <term>
```

两个服务都会在应用 `grep` **之前**脱敏掉标着 token/secret/password/API-key 的值、
bearer 凭据，以及长度 ≥ 32 的类 base64 字符串——所以拿一个 secret 当 grep 关键词，
是问不出它在不在里面的。`--follow` 是刻意不支持的；这个命令会直接报错，
而不是假装自己在流式输出。

## 发布分发

`/dl` 是一份白名单，不是目录列表：relay 只提供离线签名清单（Ed25519）点名的那些产物，
每次读取时再验一遍；其余一律 404。安装脚本是信任引导上的例外——
它们不得不以未签名的形式提供，所以对要紧的机器，优先从项目的发布页去取，
再带外比对 SHA-256。安装脚本验的是 RSA 签名而不是 Ed25519，
因为原生的 macOS LibreSSL 和 PowerShell 5.1 验不了 Ed25519——见
`docs/release-signing.md`。

`wanctl update` 会验证清单签名、换掉二进制，并且——在那些可能由某个 supervisor
以另一个账户持有运行中 agent 的平台上——检查自己到底能不能真的终止旧进程，
而不是在旧版本还在服务的时候报告成功。任何一次升级之后，
去信运行进程的启动时间对上二进制的 mtime，别信磁盘上的版本号。

## 局域网快速通道（可选）

设备可以额外保持一条通往内网 WebSocket relay 的上行（`WANCTL_LAN_RELAY`；不设即关闭）。
控制端用 `wanctl net wan|lan|auto|status` 切换（会持久化；`auto` 探测局域网 relay 的
`/healthz`；显式的 `WANCTL_RELAY` 永远优先）。局域网拨号绕过 `HTTP(S)_PROXY`
环境变量，因为公司代理会把私有网段黑洞掉。没有数据库的局域网 relay 可以把令牌拿到主
relay 去解析（`WANCTL_UPSTREAM_RELAY` + `WANCTL_ADMIN_SECRET`，5 分钟缓存），
这样门户签发的令牌在哪儿都能用。`ws://` 的局域网 relay 只有在加密覆盖网
（WireGuard 之类）里面才可以接受；否则请用 `wss://`。

## 现场笔记（都是踩出来的，别再踩一遍）

- **PaaS 的边缘会骗你两次**：剥掉 WebSocket 升级的那个边缘，同样会缓冲流式响应，
  并且无视 `X-Accel-Buffering`。长轮询（每个响应都有限）是两样都能扛过去的那个形状。
- **`/healthz` 可能被顶掉**：平台边缘可能拿自己的 JSON 来回答它；
  要测可达性，去探一条真实的应用路由。
- **当平台把一个 secret 藏起来的时候**（打码的 `DATABASE_URL`、只写不读的环境变量编辑器），
  改架构，别去捞它——门户即代理这个设计之所以存在，就是因为门户拿不到那个 DSN。
- **SSO 身份头是代理做出的一个断言。** 代理必须剥掉客户端自己带的同名头；
  应用自己没法认证它。
- **通过 ssh 让进程脱离**：`setsid CMD & exit 0` 有时候会死。用
  `nohup setsid CMD >log 2>&1 </dev/null & disown`。
- **免密 sudo 会改变安装器的语义**：早先某次 sudo 安装留下的、属主是 root 的
  `/usr/local/bin/wanctl`，会让后来一次非 sudo 的重装静默退回到 `~/.local/bin`。
  用 `WANCTL_BIN` 钉死它。
- **一个什么都不打日志的重试循环，是世上最难调的那种故障。**
  agent 的轮询循环会记下第一次失败，之后大约每分钟一条，然后记下恢复。
  任何一个会永远重试下去的循环，都照这条办。
- **一句「已在设备上验证过」，只值它当时用的那个调用形式。**
  验证一个新平台，要跑真正发布出去的那个安装器，然后从 PATH 上敲裸命令——
  不是手工拷过去的一个二进制。
- **Android 打破了四条 Unix 假设**（解析器、HOME、/bin/sh、目录穿越权限），
  Termux 又多打破两条（linker 会重复 argv、不能从 app 私有目录执行）。
  完整的账，包括为什么 APK 的 `lib/<abi>/` 目录是一个 app 唯一能执行的地方，
  都在 `docs/android.md` 里。

## 本地冒烟测试（不依赖任何外部服务）

```bash
go build -o /tmp/wanctl .
WANCTL_TOKENS="tk1:teamA" /tmp/wanctl relay --addr :18080 &
WANCTL_CONFIG_DIR=/tmp/wc-agent /tmp/wanctl agent --relay ws://127.0.0.1:18080 --token tk1 --name lab-pc --yes &
WANCTL_CONFIG_DIR=/tmp/wc-ctl WANCTL_RELAY=ws://127.0.0.1:18080 WANCTL_TOKEN=tk1 /tmp/wanctl peers
WANCTL_CONFIG_DIR=/tmp/wc-ctl WANCTL_RELAY=ws://127.0.0.1:18080 WANCTL_TOKEN=tk1 /tmp/wanctl exec --target lab-pc "echo ok"
```
