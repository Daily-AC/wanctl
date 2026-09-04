# 环境变量参考

这份参考是从生产 Go 源码里的环境变量读取处，以及它那些需要显式打开的实网测试里推导出来的。
空值通常等同于没设。标着「视情况」的变量，只有用到那个功能时才是必需的。

## 服务端与容器

| 变量 | 角色 | 必需 | 默认值 | 用途 |
|---|---|---:|---|---|
| `WANCTL_ROLE` | 容器 | 否 | `relay` | Docker 镜像的启动角色：`relay`、`portal` 或 `mcp`。 |
| `DATABASE_URL` | relay | 视情况 | 无 | PostgreSQL DSN。带门户的多用户部署必需；否则 relay 需要 `WANCTL_TOKENS` 或 `WANCTL_UPSTREAM_RELAY`。 |
| `WANCTL_AUTO_MIGRATE` | relay | 否 | 开启 | 设成 `0` 可跳过内嵌的数据库 migration。 |
| `WANCTL_ADMIN_SECRET` | relay、portal、管理 CLI | 视情况 | 无 | `/admin/*` 的共享 secret；设了的话 relay 启动时要求至少 32 字节。门户可用、上游令牌解析、管理 CLI 和服务端日志访问都需要它。 |
| `WANCTL_TOKENS` | relay | 视情况 | 无 | 逗号分隔的静态 `token:namespace` 对；给没有 Postgres 的 relay 兜底用。 |
| `WANCTL_UPSTREAM_RELAY` | relay | 视情况 | 无 | 本 relay 没有数据库时，拿去解析令牌的上游 relay URL。需要 `WANCTL_ADMIN_SECRET`。 |
| `WANCTL_PORTAL_NS` | relay | 否 | 无 | 允许开启特权门户控制台会话的命名空间。惯例是 `portal`。 |
| `WANCTL_DIST_DIR` | relay | 否 | `/dist` | 存放签名过的发布产物和安装器的目录。 |
| `WANCTL_PUBLIC_ORIGIN` | relay | 视情况 | 无 | relay 的规范 origin，会被替换进 `/skills`，以及从 `/install.sh` 和 `/install.ps1` 提供的安装器里，这样从这台 relay 取到的脚本就从这台 relay 安装。绝不从请求的 Host 推导：没设时 `/skills` 返回 503，安装器则原样带着它内置的 base 提供。 |
| `WANCTL_MCP_SEED` | relay、MCP | 视情况 | 无 | 十六进制种子，在 relay 上启用 `/wanctl-mcp`；独立跑 `mcp --http` 时必需，且解码后至少 32 字节。 |
| `WANCTL_MCP_LOCAL_ROOT` | MCP stdio | 否 | 进程工作目录 | `wanctl_push` 和 `wanctl_pull` 唯一可以访问的本地目录树。wanctl 配置目录永远被排除在外。 |
| `WANCTL_MCP_ALLOWED_ORIGINS` | MCP HTTP | 否 | 无 | 逗号分隔的浏览器 Origin 白名单。带 Origin 的请求不在名单里就拒绝；程序化的客户端通常一个都不带。 |
| `WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER` | MCP | 否 | `0` | 只有想恢复「模型可调用的设备 TOFU 钉扎」时才设成 `1`。默认是失败即关闭，因为模型分不清一个独立验证过的指纹和一个由敌意 relay 递过来的指纹。 |
| `RELAY_ADMIN_URL` | portal | 是 | 无 | 门户的管理代理所用的 relay 内网基址，比如 `http://relay:8080`。 |
| `WANCTL_GITHUB_CLIENT_ID` | portal | 视情况 | 无 | 启用 GitHub OAuth 登录。与 `PORTAL_USER_HEADER` 互斥。 |
| `WANCTL_GITHUB_CLIENT_SECRET` | portal | 视情况 | 无 | OAuth App 的 client secret；设了 client ID 就必需。 |
| `WANCTL_SESSION_SECRET` | portal | 视情况 | 无 | OAuth state 和会话 cookie 的 HMAC 密钥；启用 OAuth 时至少 32 字节。 |
| `WANCTL_GITHUB_AUTH_BASE` | portal | 否 | `https://github.com` | OAuth 授权/令牌的基址；GitHub Enterprise 用得上。 |
| `WANCTL_GITHUB_API_BASE` | portal | 否 | `https://api.github.com` | GitHub 用户 API 的基址；GitHub Enterprise 用得上。 |
| `PORTAL_USER_HEADER` | portal | 视情况 | `X-Auth-Request-Email` | header 认证模式下，来自可信反向代理的身份头。代理必须剥掉客户端自带的同名头。与 GitHub OAuth 互斥。 |
| `PORTAL_PUBLIC_ORIGIN` | portal | 否 | 由请求推导 | 门户对外的 origin，用于 OAuth 重定向和安全 cookie。TLS 在代理上终结时要设。 |
| `PORTAL_DEBUG_WHOAMI` | portal | 否 | `0` | 设成 `1` 打开诊断用的 `/whoami` 端点。不要常开。 |
| `WANCTL_RELAY` | portal | 视情况 | 持久化配置，其次构建时默认值 | 门户控制台和 `/skills` 跳转所用的公网 relay URL。客户端和 agent 也用它，可以用 `wanctl config set relay=…` 持久化。 |
| `WANCTL_PORTAL_TOKEN` | portal | 视情况 | 无 | `WANCTL_PORTAL_NS` 里的令牌；只有实时设备控制台才需要。 |
| `WANCTL_TRANSPORT` | portal、agent、控制端、MCP | 否 | `http` | 载体：与代理无关的 `http` 长轮询，或者 `ws`。 |
| `WANCTL_CONFIG_DIR` | 所有有状态的角色 | 否 | 操作系统的用户配置目录 | 存放身份、信任、令牌、标签、日志和进程状态的目录。容器镜像里设成 `/data`。 |
| `WANCTL_LARK_APP_ID` | portal | 否 | 无 | 遗留的可选 Lark 审批集成；只有配套的 secret 也设了才生效。 |
| `WANCTL_LARK_APP_SECRET` | portal | 否 | 无 | 与 `WANCTL_LARK_APP_ID` 配对的 secret。 |

## agent 与控制端

| 变量 | 角色 | 必需 | 默认值 | 用途 |
|---|---|---:|---|---|
| `WANCTL_PORTAL` | agent、控制端 | 视情况 | 持久化配置，其次构建时默认值 | 登录/接入和配对链接所用的门户 URL。用 `wanctl config set portal=…` 持久化。 |
| `WANCTL_RELEASE_BASE` | agent、控制端、安装器 | 否 | `wanctl config set release_base=…`，其次构建时默认值 | 签名过的发布产物平铺存放的基址（官方构建烤进的是项目的 GitHub releases）。`wanctl update` 和安装器都从这里拉；为空则退回 relay 的 `/dl` 镜像。二进制运行的地方够不到那个烤进去的发布页时，用 `wanctl config set release_base=https://relay.example.com/dl` 把它持久化。 |
| `WANCTL_DIST_BASE` | 安装器 | 否 | 无 | 仅安装器可用的产物来源覆盖项；优先级高于 `WANCTL_RELAY` 和烤进去的发布基址。 |
| `WANCTL_TOKEN` | agent、控制端、MCP | 视情况 | 已保存的令牌，或无 | 命名空间 bearer 令牌。覆盖配置目录里存着的那个。 |
| `WANCTL_LABEL` | 控制端、MCP | 否 | 已保存的标签，或生成的 MCP 标签 | 配对时显示的、人能读的控制端身份。 |
| `WANCTL_LAN_RELAY` | agent、控制端 | 否 | 构建时默认值，或无 | 局域网快速通道用的可选内网 WebSocket relay。 |
| `WANCTL_PORTAL_FPS` | agent | 否 | 无 | 逗号分隔的门户管理员指纹，用于播种。 |
| `WANCTL_PORTAL_FP` | agent | 否 | 无 | 已废弃的单指纹别名，只有 `WANCTL_PORTAL_FPS` 没设时才用。 |

## Android 专有

| 变量 | 角色 | 必需 | 默认值 | 用途 |
|---|---|---:|---|---|
| `WANCTL_DNS` | Android agent/控制端 | 否 | 系统/Termux 解析器，其次内置的公共解析器 | 逗号分隔的 DNS 服务器地址；只写地址则用 53 端口。 |
| `WANCTL_ELEVATION` | Android agent | 否 | 关闭 | 真值（`1`、`true`、`yes` 或 `on`）打开 Android 的 `su`/ADB 提权通道。 |
| `WANCTL_ADB_PORT` | Android agent | 否 | 发现到的端口，其次 `5555` | 逗号分隔的本地 adbd 端口，先于其它来源尝试。 |
| `WANCTL_DEVICE_NAME` | Android agent | 否 | ADB 身份用 `wanctl-agent` | 嵌进本地 ADB key 标签里的名字。通常由 Android app 注入。 |
| `WANCTL_DEVICE_STATE_FILE` | Android agent | 否 | 无 | Android app 写的 JSON 状态文件，装着电池数据和 adbd 发现结果；通常由 app 注入。 |

## 发布工具链

| 变量 | 角色 | 必需 | 默认值 | 用途 |
|---|---|---:|---|---|
| `WANCTL_RELEASE_SIGNING_KEY` | 发布清单工具 | 视情况 | 无 | Base64 的 Ed25519 种子或私钥；签一个发布必需。 |
| `WANCTL_RELEASE_RSA_KEY` | 发布清单工具 | 视情况 | 无 | Base64 的 PKCS#8 或 PKCS#1 RSA 私钥（至少 2048 位），用于安装器签名。 |

## 构建与安装

| 变量 | 角色 | 必需 | 默认值 | 用途 |
|---|---|---:|---|---|
| `WANCTL_VERSION` | Docker 构建 / compose | 否 | `dev` | 打进自部署 relay 或门户镜像里的版本号。设成检出的那个 tag，或者 `git describe --always`。 |
| `WANCTL_RELEASE_PUBLIC_KEYS` | Docker 构建 / compose | 否 | 无 | 逗号分隔的 Ed25519 公钥，烤进自部署镜像，好让 relay 的 `/dl` 能验签名过的发布。 |
| `WANCTL_DEFAULT_PORTAL` | Android 构建 | 否 | 无 | 烤进 APK 的门户 origin；留空则由运行时的登录对话框负责收集。 |
| `WANCTL_BIN` | 安装器 | 否 | 视平台而定 | 装好的 `wanctl` 可执行文件的落盘路径。 |

## 需要显式打开的实网测试

这些变量不是运行时的服务配置。它们让那些会接触真实外部系统的测试保持关闭，
除非运维显式地把所有必需值都填上。

| 变量 | 角色 | 必需 | 默认值 | 用途 |
|---|---|---:|---|---|
| `WANCTL_LIVE_RELAY` | client 实网测试 | 视情况 | 无 | 远程控制台端到端测试用的 relay URL。 |
| `WANCTL_LIVE_DEVTOK` | client 实网测试 | 视情况 | 无 | 测试设备命名空间的令牌。 |
| `WANCTL_LIVE_PORTALTOK` | client 实网测试 | 视情况 | 无 | 特权门户命名空间令牌。 |
| `WANCTL_LIVE_TARGET` | client 实网测试 | 否 | `alice/macbox` | 实网测试用的全限定目标。 |
| `WANCTL_LARK_TEST_EMAIL` | Lark 实网测试 | 视情况 | 无 | 真实审批卡片探针的收件人。 |
| `WANCTL_LARK_KEEP_CARD` | Lark 实网测试 | 否 | `0` | 设成 `1` 则把探针卡片留成可操作状态，而不是立刻结掉它。 |
| `WANCTL_LARK_LIVE_SECONDS` | Lark 实网测试 | 视情况 | 无 | 保持真实回调消费者连接的秒数，正数。 |

`EDITOR`、`PREFIX`、`TMPDIR` 这类标准进程变量不属于 wanctl 自己的这套 API。
