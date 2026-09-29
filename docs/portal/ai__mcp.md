MCP 是 AI 直接调 wanctl 的接口——不用它读 skill、不用它拼命令行，`wanctl_peers`、`wanctl_exec` 这些工具就长在它的工具列表里。这些工具有两种接法，选哪种只看一件事：**那个 AI 能不能在它自己那台机器上起一个 `wanctl` 进程。**

| | 用哪种 | 典型的 |
| --- | --- | --- |
| 能起本地进程 | 本机 stdio | Claude Code、Codex、Cursor |
| 起不了 | 公网端点（宿主要支持 OAuth） | ChatGPT、claude.ai 网页版、云端 agent 运行器 |

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

## 公网接法（HTTP，OAuth）

端点是 `https://relay.example.com/mcp`。有些 relay 答在 `/wanctl-mcp` 上，因为它前面的代理占掉了 `/mcp` 前缀；用哪个问部署方一句。它不需要你机器上的任何文件，也不需要你把令牌复制给它。

鉴权只有 OAuth 一种。宿主第一次连上来会收到 401，响应里带着授权发现地址，它顺着找到授权服务器，把你带到门户：照常用 GitHub 登录，页面上写着是哪个客户端在申请、授权后会跳回哪个域名，你点一下「允许」就结束了。没有 code 要复制，也没有来回粘贴。

- **Claude Code**：`claude mcp add --transport http wanctl https://relay.example.com/mcp`，然后在 Claude Code 里输入 `/mcp`，选中 wanctl 走一遍授权。
- **ChatGPT、claude.ai 这类网页 AI**：在自定义连接器里填这个 URL，鉴权方式选 **OAuth**（不要选「无鉴权」），剩下的发现步骤它自己走完。

> 这个端点是公开的，但没带授权的请求一律 401：握不了手，也看不见任何设备。

授权跟着这个连接器走，不跟着会话走，所以它每次新开会话都还是已登录状态；同一个端点上别的人、别的连接器各自授权，谁也看不到谁的设备。想收回：去门户的访问令牌页，把那条标着 `oauth:` 加客户端名的令牌吊销掉；或者让 AI 调一次 `wanctl_logout`，效果一样。连接器一直在用，授权会自己续期；闲置满 30 天，这份授权连同那条令牌一起失效，要再用就重新授权一次。

授权之后它第一次拨某台设备，那台设备的网页「待审批」里会冒出一条**配对请求**，署名是「AI 助手 · MCP 会话」。你点「信任它」，它才连得上——之后每条命令仍然照你那台设备的模式走审批，跟别的控制端一模一样。

> v0.19.0 起公网端点只认 OAuth。以前不用 OAuth 的那条路（`wanctl_login` 取一次性 code、`wrb1.` 开头的 rebind 凭证）已经删掉：它的登出只记在 relay 内存里，relay 一重启，登出过的凭证又能用。不支持 OAuth 的第三方托管 AI 用不了公网端点；能在自己机器上起进程的 AI，用上面的本机 stdio 接法。
>
> 公网端点要部署方在 relay 上同时备齐数据库、`WANCTL_PUBLIC_ORIGIN` 和 `WANCTL_PORTAL`。缺任何一样，`/mcp` 直接回 503，说明缺的是哪一样。

## 它能碰到什么、碰不到什么

- **身份跟着授权走。** 每个请求都带着 OAuth 访问令牌，它指明是哪个账号；门户上吊销或 `wanctl_logout` 之后，下一个请求就是 401。
- **传文件只能往上传。** `wanctl_push` / `wanctl_pull` 在公网端点上是关掉的——那个「本地路径」会是服务器上的路径，不是你的。要给设备送文件用 `wanctl_push_blob`。
- **换种子等于全体重新授权。** 访问令牌和存下来的授权都用 relay 的 MCP 种子密封；部署方换掉种子，所有授权立刻作废，每个连接器都得重新授权一次。
- **不想给了就说一声。** 让 AI 调 `wanctl_logout`，或者去门户的访问令牌页吊销，这份授权立刻失效。设备那边的信任要撤，去设备页面上撤。

## v0.12.0：保持远程工作区

让 AI 调用 `wanctl_workspace`，以 `action=enter`、目标设备和项目绝对路径打开工作区。后续读、写、编辑、执行和轮询都携带返回的 `workspace` 引用，目录、环境变量和命令结果保留在设备上。每个聊天使用自己的引用；OAuth 只保存身份，不共享聊天的当前目录。

本地宿主确定每个对话独占一个 MCP 进程时，可以用 `wanctl mcp --workspace-session` 自动绑定工作区。CLI 使用 `wanctl workspace enter`，再传 `--workspace` 或设置该终端的 `WANCTL_WORKSPACE`。相同本地控制端身份的 CLI 与 stdio MCP 可以接续；网页 OAuth 身份独立。

断连后查询原请求 ID，不要换新 ID 重做未知结果的操作。完成后用 `wanctl_workspace action=exit` 关闭；工作区不跨受控 agent 重启保存。控制端、relay 和受控端均需升级。完整用法见 [工作区文档](https://github.com/Daily-AC/wanctl/blob/main/docs/workspaces.md)。

> 首次设备信任仍遵循宿主的批准流程。部署方开启相应能力后，可以通过 `wanctl_trust_server` 记录已核验的指纹；已有授权够用时继续，否则先获得必要确认。指纹变化时必须停止并核实原因，不能自动覆盖旧指纹。该能力需要部署方显式启用，未启用时使用本地 stdio 的信任流程。
