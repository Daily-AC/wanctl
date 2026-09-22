# 05｜故障与权限：结果不明时，先做什么

<div class="course-goal"><strong>这一篇的收获</strong><p>遇到失败时，能够区分“需要查状态”“需要授权”和“需要结束工作”，不凭一条报错猜测远端发生了什么。</p></div>

## 一个没有收到结果的命令

设备已经给文件追加了一行，返回结果时网络断开。调用方只知道自己没收到回复，不能因此断言命令没运行。如果换一个新编号再提交，追加动作可能发生第二次。

工作区为命令保存请求编号和执行记录。账本仍在时，相同编号与相同输入会对应原任务；相同编号却换了命令内容会被拒绝。重连后应先查询原编号。[项目实现：请求登记与冲突检查](https://github.com/Daily-AC/wanctl/blob/v0.12.1/internal/agent/workspace.go)

<figure class="course-map"><div class="course-lanes two"><section><strong>调用方知道的事</strong><p>没有收到结果。执行是否完成仍然未知。</p></section><section><strong>设备可能知道的事</strong><p>原请求已登记，正在运行或已有最终结果。</p></section></div><figcaption>恢复的第一步是对齐已有记录，不是猜测并重做。</figcaption></figure>

这不是跨设备重启的“恰好执行一次”保证。agent 重启丢失账本后，不能用新建的空记录假装记得旧任务。命令编号也不同于 JSON-RPC 用来匹配请求与响应的消息 ID，不能直接拿会变化的通信编号代替。[原始资料：JSON-RPC 请求对象](https://www.jsonrpc.org/specification#request_object)

## 按失败类型选择下一步

| 观察到的情况 | 下一步 |
| --- | --- |
| 网络中断，命令结果未知 | 用原工作区与命令编号查询；不换新编号盲目重做 |
| 文件写入结果未知 | 读取文件、核对内容和摘要，再决定是否重试 |
| 文件摘要冲突 | 重新读当前文件，再基于它准备修改 |
| 权限被拒绝、撤销或到期 | 按拒绝来源取得必要授权；不换工具绕过 |
| 设备身份与已固定指纹不一致 | 停止并核实设备身份 |
| 工作区已关闭或设备端状态丢失 | 明确结束旧工作；不悄悄回退本地或另一设备 |

文件写入的未知结果与命令去重不是同一种机制：不能假定给所有文件操作加一个相同编号，就自动拥有命令账本的保证。[文件操作的未知结果处理](https://github.com/Daily-AC/wanctl/blob/v0.12.1/internal/client/fileops.go)

## 等待、取消、退出是三件事

停止等待只表示调用方暂时不再接收结果。`cancel` 要求终止命令；当前工作区实现会使相应 shell 失效，不能承诺保留它的全部环境。`exit` 则明确结束整个工作区。

此外，宿主的工具审批、wanctl 的身份与访问检查、设备的执行策略位于不同位置。一个层面的允许不能替代另一个层面的检查。工作区根目录只帮助解释路径，也不是限制任意程序访问整个系统的沙箱。[MCP 宿主职责](https://modelcontextprotocol.io/specification/2025-06-18/architecture#host) · [项目权限边界](https://github.com/Daily-AC/wanctl/blob/v0.12.1/docs/adr/0014-cli-workspace-binding.md)

## 自检：先判断，再看解释

<div class="course-check" data-correct="0">
<p class="course-question">命令提交后连接断开，你还保存着原请求编号。最合适的第一步是什么？</p>
<div class="course-choices">
<button type="button" data-choice="0" aria-pressed="false">查询原任务</button>
<button type="button" data-choice="1" aria-pressed="false">换编号重跑</button>
<button type="button" data-choice="2" aria-pressed="false">切本地继续</button>
</div>
<p class="course-feedback" role="status" aria-live="polite"></p>
<details><summary>查看解释（可以跳过作答）</summary><p>先查询原任务才能区分正在执行、已经完成和记录不可用。换新编号可能重复副作用；切回本地则可能换了工作地点。</p></details>
</div>

## 带着什么进入下一篇

每一种恢复动作都应依据已知事实。下一篇把这些边界变成可核验的验收条件。

如果这一点还不清楚，可以把本篇的问题和你自己的项目场景交给协作 agent，请它换一个例子解释。自检只提供即时反馈，不代表已经掌握；隔一段时间，不看答案再解释一次更有价值。

[术语速查](reference/terms.md) · [架构卡片](reference/architecture-card.md) · [课程目录](../../portal/learning__remote-workspace.md)
