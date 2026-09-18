# WebFetch 接入层

让能够读取指定 URL 的网页 AI 使用 wanctl，无需原生 MCP 连接器。这是中继旁的可选控制端
适配器：设备主人在现有门户批准短期授权，设备自己的信任、规则、模式和逐次审批仍决定操作。

同一份协议服务于网页读取工具、Python/JavaScript HTTP 客户端及未来的 SDK、MCP 适配层。
权限逻辑不依赖 AI 平台或客户端语言。客户端必须能够读取指定 HTTPS URL 的实时响应，
仅搜索已有索引不等于具备这项能力。

门户的 `/webfetch/help` 是公开、无需登录的快速调用说明。所有协议响应都包含 `help_url`，
接入提示词也带上这个完整地址。已批准的 HTML 响应开头直接展示 GET 调用地址、设备 target
和各工具 URL 模板，完整 JSON 保留在后面。AI 可使用原有网页读取工具执行填好的 GET URL，
无需额外的 exec 连接器。就绪回复应保留完整 `status_url` 和 exec 模板，供后续对话继续使用。

## 主人使用流程

网页版建议先在已登录门户打开“设置 → 连接网页 AI”（`/webfetch/connect`）。每次复制都会用
密码学随机数生成一段带完整网址的接入提示词，不要求模型编随机值或记住隐藏的工具结果。
打开或复制该页面不会授予权限，每个新对话应重新复制。SDK 客户端也可以自行读取发现页、
使用安全随机数生成器，按以下流程接入。

1. 让 AI 打开 `https://RELAY/webfetch/v1`。静态发现页不含票据。客户端生成本次对话独立的
   `client_nonce`（24 个新密码学随机字节，以 48 位小写十六进制表示），填入 `start_url_template`，
   再 GET 这个唯一地址。返回的 `client_nonce` 必须与自己的一致，否则说明抓取端返回了其他申请。
2. AI 返回 `approval_url` 和 `continuation_prompt`。你在自己的浏览器打开授权链接，登录
   wanctl，核对控制端和所选设备身份，选择设备及有效期。读取授权链接本身不会批准申请。
3. 批准后将 `continuation_prompt` 发回 AI。它含有完整 `status_url`：部分网页聊天不会保留
   上轮工具结果，也不会因一句“已批准”就启用网页读取。状态页给出完整的 `devices[].target`、
   工具参数 schema 和 URL 模板。客户端必须照抄 target，不应猜测其格式。
4. 首次使用设备时，可能还需进行原有的控制端配对。AI 应转交给主人操作的链接，不能自行审批。
   配对不会改变设备操作规则，也不会开启 bypass。
5. 用完后，在“设置 → 访问令牌”吊销授权。

支持 Agent Skill 的聊天不必每个对话都走这一遍。门户在 `/webfetch/skill` 提供一份：无需登录的
Markdown 文件，带 YAML frontmatter（`name: wanctl-webfetch`），其中的中继与门户地址已替换成本
实例自己的，连接页有入口，发现页也以 `skill_url` 公布。把它粘贴进 Claude 项目、自定义 GPT 或
任何有技能设置的宿主，就替代了每次复制的接入提示词：AI 已经知道从本中继的 `/webfetch/v1`
开始，主人只需说要做什么。该地址不含任何凭据、不授予任何权限；未启用 WebFetch 时返回 404。

首版只允许选择有固定 ID 和已登记指纹的自有设备。原有跨账号共享不变。设备改名不改变授权；
设备移除或证书变化会使授权失效。临时授权只有设备使用权，没有控制台或设备管理权。

设备运行模式是最终依据：授权使用处于 bypass 模式的设备，也会授予该设备现有的广泛使用能力。
WebFetch 不会声称能把 exec 权限和任意 shell 命令实际能做的事分开。

## 管理员配置

升级中继和受控端。旧受控端没有声明委托会话校验能力时，连接会被拒绝。门户也需要升级，
以提供审批页面。

默认关闭 WebFetch，设置 `WANCTL_WEBFETCH_SEED` 后启用。配置项如下：

| 变量 | 含义 |
| --- | --- |
| `DATABASE_URL` | 现有 wanctl PostgreSQL 数据库，用于持久授权与请求去重 |
| `WANCTL_WEBFETCH_SEED` | 至少 32 个解码字节的十六进制秘密种子，保存在管理员的秘密存储中 |
| `WANCTL_PUBLIC_ORIGIN` | AI 可见链接使用的公网 HTTPS 中继地址 |
| `WANCTL_WEBFETCH_PORTAL_ORIGIN` | 主人登录审批使用的公网 HTTPS 门户地址 |
| `WANCTL_WEBFETCH_RELAY_URL` | 可选的适配器到中继地址，默认公网中继地址；仅接受 HTTPS 或回环 HTTP |

自部署 Compose 会传递这些配置。通过受保护的环境文件提供种子，复用 `PORTAL_PUBLIC_ORIGIN`
和中继容器的回环地址。例如调用 Compose 时同时指定
`--env-file .env --env-file /secure/webfetch.env`。后续部署也必须使用同一受保护文件，
避免漏传种子导致适配器关闭。

授权有效期间保持种子不变。带用途区分的派生算法为每份申请生成控制端身份和中继凭证。
网页只收到临时浏览器票据，不会收到种子、私钥、账号 Token、门户 Token 或原始中继委托凭证。
PostgreSQL 只保存凭证哈希。

公网 `/webfetch` 路径必须能被网页 AI 的抓取服务访问，不能依赖主人浏览器的 Cookie。
批准操作仍在已登录的门户内进行，保留现有 CSRF 保护。

入口代理必须对 `/webfetch/` 的访问日志和带请求地址的错误日志做脱敏或关闭该路径的记录，
因为路径含票据、查询参数含操作内容。例如 nginx 对该路径使用 `access_log off` 和
`error_log /var/log/nginx/webfetch.error.log crit`。不要全局关闭诊断日志。
应用日志不记录票据或中继凭证。响应设置 `no-store`、`no-referrer` 和 `noindex`；
第三方抓取服务的数据保留行为不由 wanctl 控制。

适配器是受信任的控制端，能看到命令与返回内容。适配器到设备仍使用 wanctl 双向 TLS；
这不等于网页模型到设备之间存在适配器也无法解读的端到端加密。

## GET 工具协议

默认响应是包含可见结构化数据的静态 HTML，增加 `format=json` 可获取 JSON。
不需要 JavaScript 或流式连接。

每个文档均包含 `protocol: "wanctl.webfetch.v1"`、流程 `status` 和 `http_status`。
普通 HTML GET 的客户端错误返回可读取的 HTTP 200 页面，同时标记 `status: "error"`
以及实际 `http_status`（400、403、409、429）。否则不少网页工具会丢弃错误正文。
**页面加载成功不代表授权或执行成功。** JSON 保留标准 HTTP 错误码。HEAD、不支持的方法
和浏览器跨域请求也保留原有 HTTP 错误。所有权限检查仍先于实际操作和结果读取执行。

### 发现协议与独立申请

`GET /webfetch/v1`（也可访问 `/webfetch`）是公共、静态、无凭证的发现页。
它也返回 `owner_start_url`，供没有安全随机数生成器的客户端交给主人操作。
返回 `start_url_template: https://RELAY/webfetch/new/{client_nonce}`。
`GET /webfetch/new/CLIENT_NONCE` 创建待批准申请，返回 `approval_url`、`status_url`
和 `continuation_prompt`。模板必须先填完再访问，字面占位符会被拒绝且不会创建申请。

不能在多人共用的公共入口中放可重复使用的会话地址：第三方抓取工具可能忽略 `no-store`
而重放缓存。每个对话应先生成独立的抓取地址，服务端再独立生成真正的随机票据。
在源站重复请求同一 nonce 也无法取回旧授权的票据。不要复用其他对话的 nonce、授权链接
或状态地址，也不要把会话地址提交到公开搜索索引。

只有已登录的主人能批准申请。批准会把申请绑定到该账号，其他账号无法接管。
已有 `/webfetch/s/TICKET` 会话在原有效期内继续使用，仍受吊销约束。

已批准的 manifest 返回 `call_endpoint`，按以下格式构造请求：

```text
GET CALL_ENDPOINT?rid=UNIQUE_REQUEST&tool=TOOL&target=CANONICAL_TARGET&...
```

`CANONICAL_TARGET` 是 `devices[].target` 给出的完整 `namespace/device_id`，不能只填
namespace、只填 ID 或使用 `namespace:ID`。每个参数值只编码一次，target 的斜杠编码为 `%2F`。
每个工具都有 `input_schema` 和 `call_url_template`，描述必填项、类型、范围和可用设备。
模板使用无法直接执行的占位符，而不是可运行的示例命令。可用工具如下：

| 工具 | 参数 | 结果 |
| --- | --- | --- |
| `exec` | `command`，可选 `cwd`、`timeout_seconds`（1–1800，默认 300） | 一次性执行、退出码和有限长度的 stdout/stderr |
| `read_text` | `path`，可选 `offset`（从 1 开始的行号，默认 1）、`limit`（行数，默认 2000） | `content`、`total_lines`、`first_line`、`last_line`、`size_bytes`、整份文件的 `sha256`、`truncated`，以及 `next_offset` 或 `long_line` |
| `edit_text` | `path`、`old`、`new`，可选 `all`、`expected_sha256` | `replaced`、`sha256`、`size_bytes` |
| `write_text` | `path`、`content` | wanctl 文件上传、字节数和 SHA-256 |

`read_text` 和 `edit_text` 是设备上的原生文件操作，不经过 shell：和 wanctl 命令行、MCP
服务端用的是同一套读取行区间与原地替换，只是换成用 GET 调用。调用方传的文本不会被任何
shell 解析；结果准确说明返回了哪几行；编辑保留文件其余部分的每一个字节——CRLF 行尾、文件
权限都不变——并通过临时文件改名原子写入。

翻页由调用方负责：只要 `last_line` 小于 `total_lines`，就用 `offset = last_line + 1` 继续读。
`truncated: true` 表示 32 KiB 的响应上限在行边界上截断了这次读取，此时 `next_offset` 指出
从哪一行继续，既不丢行也不重复。`long_line` 指出某一行本身就大到无法完整返回；再读一次
只会拿回同样的前缀，这一行要改用 `exec` 读。读取返回的 `sha256` 是整份文件的哈希，把它作为
编辑的 `expected_sha256` 传回，就能证明改的正是刚才读到的那份文本。

改动已存在的文件要用 `edit_text` 而不是 `write_text`：后者整份覆盖，会悄悄丢掉你上次读取之后
别人写进去的内容。编辑的 `old` 和 `new` 走在 URL 里，因此限制它的是 8 KiB 的 URL 上限而不是
另设的阈值，一次改一段。被拒绝的编辑（`old` 找不到、`old` 出现多次而没有 `all=true`、
`expected_sha256` 已经对不上）不会替换目标文件，返回 `error_code: "file_refused"` 和
`execution_started: false`，并带上出现次数或文件当前的 `sha256`；修正后用**新的 rid** 重新提交。
读取受设备的读权限约束，编辑受写权限约束，与 `read_text`、`write_text` 完全一致；两者都会以
`READ`／`EDIT` 加路径的形式记入设备事件日志。

响应包含 `job_id` 和 `result_url`。运行中的任务还返回新的 `next_url`、建议的轮询间隔
`poll_after_seconds` 和该任务自己的 `deadline_at`，读取它直到状态变成 `done`、`failed`
或 `unknown`。`deadline_at` 取「请求的超时」和「授权结束时间」中较早的那个，判定运行中的任务
是否变成 `unknown` 用的也是同一个值，所以真的要跑几分钟的编译或渲染会保持 `running`。
`unknown` 并不只对应「超时」：传输中断、输出超限和适配器内部异常同样记为 `unknown`。
它的含义是适配器在记下结果之前失去了这个任务的踪迹——操作可能已经执行。适配器异步处理任务，但调用的是 wanctl
原生同步一次性操作，不会向委托客户端暴露设备端持久 shell 或脱离连接的异步任务
（设备会拒绝委托会话上的 `exec_async`/`exec_poll`，见 `docs/adr/0009-webfetch-long-jobs.md`）。

失败任务可能包含 `result.pairing_url`。批准设备使用权不等于控制端已配对。把该链接和
当前的 `continuation_prompt` 交给主人后停止。参数错误会说明拒绝原因、指向可读取的
授权 manifest，且不创建任务。不要枚举 target 格式或改成 POST。写空文件也必须显式传 `content=`。
`pairing_required` 会明确返回 `execution_started: false`。主人完成配对后，应使用新的 rid
发起新尝试；已经完成的失败任务不会改变，复用旧 rid 不会再次执行操作。

`rid` 在一份授权内唯一。同一 rid 和参数返回同一任务，换参数则返回 409。
持久账本先于执行落库，所以重复抓取或重启适配器不会自动重放操作。
响应丢失时，请用同一个 `rid` 和同样的参数重新读取完全相同的网址：这会返回已经记录下来的
那个任务，而不会重复执行；返回的是这个任务**已经记录下来的**结果而不是重新执行一次，
所以文件改动之后重放同一次读取，拿到的仍是第一次读到的文本。`edit_text` 的会话在没有收到
回复时记为 `unknown` 而不是拒绝：从控制端看，「已经写入但回复丢了」和「设备太旧不认识这个动作」
是同一种现象。只有在结果明确表示「什么都没执行」时才换新的 rid
（`pairing_required`、`adapter_busy`，或被拒绝的读取／编辑 `file_refused`——设备已经做出
判断且没有替换目标文件，三者都带 `execution_started: false`），
不带这个标记的 `failed` 和 `unknown` 之后都不可以。中断的请求即使没有结果也可能已经产生副作用；
`unknown` 意味着主人应先检查设备记录，再决定是否创建新请求。
这不是对任意外部副作用的“恰好执行一次”保证。

限制：待审批申请 10 分钟过期；批准后有效期 1–60 分钟、最多 16 台设备；每份授权 64 个任务。
`exec` 的 `timeout_seconds` 含排队为 1–1800 秒、默认 300 秒，编译、安装和渲染因此能跑完而不是
被判超时；`read_text`、`edit_text` 和 `write_text` 仍是 1–60 秒、默认 30 秒——输出不超过 32 KiB、
URL 不超过 8 KiB 的文件操作如果慢，那就是卡住了。任务截止时间还会被授权的剩余时长截断，
所以主人批准的时长要长过任务本身。
并发上限是每份授权 4 个操作、整个适配器 64 个；超出时调用立即失败，返回
`error_code: "adapter_busy"` 和 `execution_started: false`。URL 最长 8 KiB，编辑的 `old` 和 `new`
也由它兜底；写入最多 2 KiB UTF-8；一次读取最多返回 32 KiB，无论 `limit` 要了多少行都在行边界
截断；stdout 和 stderr 分别最多 16 KiB，超限会取消任务。HEAD 不创建或执行任务。

浏览器票据有不可延长的 70 分钟上限。失效授权与任务内容在至少 24 小时后清理，
旧票据不能重新创建已删除申请。原有账号与设备审计另行保留。

会话和状态链接本身是短期凭证，不要公开仍有效的聊天记录；应先吊销授权再分享。
诊断日志只记录服务端生成的申请/任务编号与固定拒绝类别，不记录票据、完整 URL、命令、
文件内容或客户端 rid。结合现有任务账本，可区分请求未到达、输入被拒、未配对和设备执行失败。

## 授权与取消边界

中继在发现设备、解析规范目标以及 HTTP/WebSocket 会话路径中检查委托 Token。
它不能注册设备、冒充受控端、访问其他凭证的会话、改变设备管理状态、签发凭证或调用普通账号管理接口。

活动连接有精确到期时间，并每秒复查授权；吊销传播还包括存储或网络延迟。
设备在人工操作审批通过后、执行或记住规则之前再次检查授权，过期或已撤销的授权不会因晚到审批复活。
任务结果读取同样要求有效授权。

关闭会话会取消新版设备上的一次性执行，但不能撤销已完成的写入，也不能保证控制一个原本有权执行的
命令主动创建的脱离连接后台进程。

**回滚到不支持此功能的旧中继之前，必须先吊销所有 `kind='delegated'` Token。**
旧的命名空间级 Token 解析器不知道这些约束。新版中继在上游检查接口中保留委托信息，
并拒绝通过旧解析器把保留的 `wfd_` 前缀降级为普通 Token。

## 开发验收

将 `WANCTL_TEST_POSTGRES` 指向一次性 PostgreSQL，运行真实授权生命周期、迁移、加密控制端/
设备连接和文件操作测试。CI 默认准备 PostgreSQL 并开启这些测试。

`go run ./tools/webfetch-demo` 提供独立的手动和浏览器验收工装，需要私有 `--state-dir`、
`--public-origin` 与一次性 PostgreSQL。只允许公开中继的 `/webfetch` 路径。
工装的主人门户仅监听回环地址并使用固定测试身份，不能发布到公网或当成正式登录配置。
工装设备采用 normal 模式，只允许测试目录和一条无害命令。
