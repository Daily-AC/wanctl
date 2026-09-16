MCP 是 AI 直接调 wanctl 的接口——不用它读 skill、不用它拼命令行，`wanctl_peers`、`wanctl_exec` 这些工具就长在它的工具列表里。同样的 17 个工具有两种接法，选哪种只看一件事：**那个 AI 能不能在它自己那台机器上起一个 `wanctl` 进程。**

| | 用哪种 | 典型的 |
| --- | --- | --- |
| 能起本地进程 | 本机 stdio | Claude Code、Codex、Cursor |
| 起不了 | 公网端点 | claude.ai 网页版、云端 agent 运行器、别人托管的 AI |

## 本机接法（stdio）

Claude Code 一行：

```sh
claude mcp add wanctl -- wanctl mcp
```

Codex 写进 `~/.codex/config.toml`：

```toml
[mcp_servers.wanctl]
command = "wanctl"
args = ["mcp"]
```

它用的就是这台机器上 `wanctl login` 已经存好的身份，装完重启一下就能用，没有另外的登录步骤。如果这台机器还没登录过，先按 [让你的 AI 来控制设备](#docs/ai-skill) 走一遍。

## 公网接法（HTTP）

端点是 `https://relay.example.com/mcp`。Claude Code 里这样加：

```sh
claude mcp add --transport http wanctl https://relay.example.com/mcp
```

别的宿主就把这个 URL 填进它的「MCP server / HTTP」那一栏。它不需要你机器上的任何文件，也不需要你把令牌复制给它。有些 relay 答在 `/wanctl-mcp` 上，因为它前面的代理占掉了 `/mcp` 前缀；用哪个问部署方一句。

> 这个端点是公开的，谁都能连上去握手——但握完手它什么设备都看不见。能看见什么，完全由下面那次登录决定。

## 第一次登录

跟 AI 说一句「登录 wanctl」，它会调 `wanctl_login`，然后照着做：

1. AI 给你一个 `https://portal.example.com/enroll` 链接，你在浏览器里打开。
2. 门户认你的账号（一般已经登录了），页面上出现一个一次性 code，5 分钟内有效。
3. 把 code 贴回给 AI。它再调一次 `wanctl_login`，这次带上 code。

登录是**一个会话一份**的：同一个端点上别的人、别的对话，各自登录各自的，谁也看不到谁的设备。

登录之后它第一次拨某台设备，那台设备的网页「待审批」里会冒出一条**配对请求**，署名是「AI 助手 · MCP 会话」。你点「信任它」，它才连得上——之后每条命令仍然照你那台设备的模式走审批，跟别的控制端一模一样。

## 会话断了怎么办

公网端点的登录态只活在 relay 的内存里。relay 重启、连接被重置，AI 就会收到「LOGIN REQUIRED」。

登录成功的时候，AI 还拿到了一串 `wrb1.` 开头的 **rebind 凭证**（7 天有效）。它自己存着，遇到这种情况直接拿这串恢复，不用再喊你开浏览器。凭证丢了也不要紧，重走一遍上面三步就是了。

## 它能碰到什么、碰不到什么

- **令牌不落盘。** 公网会话的凭证只在 relay 内存里，没有任何一份写到磁盘上。
- **传文件只能往上传。** `wanctl_push` / `wanctl_pull` 在公网端点上是关掉的——那个「本地路径」会是服务器上的路径，不是你的。要给设备送文件用 `wanctl_push_blob`。
- **换种子等于全体登出。** 部署方换掉 relay 的 MCP 种子，所有会话和所有 rebind 凭证立刻作废。
- **不想给了就说一声。** 让 AI 调 `wanctl_logout`，这个会话的凭证和它的 rebind 凭证一起失效。设备那边的信任要撤，去设备页面上撤。

> 公网端点上的会话没有自己的信任库，所以它第一次连某台设备时可能停在「设备身份确认」上，报一个它没法替你确认的指纹。这时候要么改用本机 stdio 接法，要么让部署方在 relay 上打开那个开关（见自建文档里的「打开托管的 MCP 端点」）。
