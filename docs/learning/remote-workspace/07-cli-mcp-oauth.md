# 07｜接入：CLI、MCP 与 OAuth 各保存什么

<div class="course-goal"><strong>这一篇的收获</strong><p>面对一个新宿主，能选择工作区引用的保存方式，并解释为什么登录身份不能代替工作区绑定。</p></div>

## 先问三个问题

| 要回答的问题 | 应保存的东西 |
| --- | --- |
| 调用方有权访问什么 | 有效的访问授权；设备身份与控制端信任另行核对 |
| 当前这段工作在哪里 | 属于这个终端或对话的工作区引用 |
| 状态实际放在哪里 | 设备端工作区、shell 和任务账本 |

OAuth 的访问令牌表示访问授权。连接器可以在授权仍有效时跨工具调用继续访问，但这不会自动恢复 shell，也不能替服务器判断是哪一个聊天的工作区。[原始资料：OAuth 访问令牌](https://www.rfc-editor.org/rfc/rfc6749#section-1.4) · [MCP HTTP 授权](https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization)

<figure class="course-map"><div class="course-lanes"><section><strong>授权</strong><p>决定可访问的资源和操作。</p></section><section><strong>对话绑定</strong><p>选择本次工作的引用。</p></section><section><strong>设备状态</strong><p>保存真正活着的 shell 和任务。</p></section></div><figcaption>三者分别持续，才能适应宿主重建进程或网络连接。</figcaption></figure>

## 按宿主条件选择

| 入口 | 怎样延续工作区 | 前提或代价 |
| --- | --- | --- |
| 多次 CLI 调用 | 每次传引用，或在当前终端设置 WANCTL_WORKSPACE | 子进程不能替父终端设置环境；不创建账号级默认工作区 |
| 专属 stdio MCP | 使用 workspace-session 模式保存绑定、复用连接 | 必须一段对话独占该 MCP 进程 |
| 共享 HTTP MCP | 每次传递本聊天保存的工作区引用 | 不能把账号、令牌或协议会话直接当聊天编号 |

本地绑定的实现不要求额外启动一个后台守护进程。连接复用则需要在每次操作重新检查有效授权；“连着”不等于撤销权限之后还能继续工作。[CLI 契约](https://github.com/Daily-AC/wanctl/blob/v0.12.1/docs/workspaces.md#cli) · [专属进程绑定](https://github.com/Daily-AC/wanctl/blob/v0.12.1/internal/mcp/conversation.go)

## 同一账号不等于同一控制端身份

当前工作区属于创建它的控制端身份。本地 CLI 与 stdio MCP 使用同一份身份配置时，可以接续工作；托管 OAuth MCP 的控制端身份与本机证书不同，所以不能仅凭“登录的是同一个人”就接管对方的 shell。

两个聊天使用不同工作区，可以避免 shell 目录和变量互相覆盖。但如果它们都被授权修改同一项目文件，仍可能发生文件冲突；这是为什么上一层还需要文件摘要检查。工作区独立不是文件系统沙箱。[身份与引用边界](https://github.com/Daily-AC/wanctl/blob/v0.12.1/docs/workspaces.md#host-integration)

## 读完以后，如何参与一次架构讨论

拿一个具体宿主，写下它是否能启动子进程、是否保证进程独占对话、是否保留工作区引用。再决定选哪种接法。如果其中一项未知，应验证宿主行为；不要依据“它是网页 AI”就断言每次调用一定新建进程。

## 自检：先判断，再看解释

<div class="course-check" data-correct="1">
<p class="course-question">两个聊天共用一个 OAuth 账号。为了避免互相改变当前工作地点，默认工作区应按什么范围保存？</p>
<div class="course-choices">
<button type="button" data-choice="0" aria-pressed="false">绑定到账号</button>
<button type="button" data-choice="1" aria-pressed="false">绑定到对话</button>
<button type="button" data-choice="2" aria-pressed="false">绑定到连接</button>
</div>
<p class="course-feedback" role="status" aria-live="polite"></p>
<details><summary>查看解释（可以跳过作答）</summary><p>应由各自对话持有工作区引用。账号可以服务多个对话，连接可以重建或被复用；它们都不能可靠地表示“当前正在做的这段工作”。</p></details>
</div>

## 带着什么进入下一篇

合上文档，尝试用“职责、状态、契约、故障、授权、证据”六个词说明自己的接入方案。再次需要细节时，先查架构卡片，再回到对应课程。

如果这一点还不清楚，可以把本篇的问题和你自己的项目场景交给协作 agent，请它换一个例子解释。自检只提供即时反馈，不代表已经掌握；隔一段时间，不看答案再解释一次更有价值。

[术语速查](reference/terms.md) · [架构卡片](reference/architecture-card.md) · [课程目录](../../portal/learning__remote-workspace.md)
