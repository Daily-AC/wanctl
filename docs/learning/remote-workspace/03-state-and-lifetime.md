# 03｜状态：断掉一条连接，会丢掉什么

<div class="course-goal"><strong>这一篇的收获</strong><p>遇到断网或重启时，先找到状态的持有者，再判断哪些东西还能继续使用。</p></div>

## 不要把所有东西都叫“会话”

一次远端工作同时包含几种寿命不同的对象。它们可能同时开始，却不需要一起结束。

| 对象 | 保存什么 | 谁持有 |
| --- | --- | --- |
| 访问授权 | 可以访问哪些资源、何时失效 | 授权系统与调用方凭据 |
| 工作区引用 | 这次工作对应哪个设备和工作区 | 调用方或独占对话的适配器 |
| 网络／MCP 会话 | 当前这段通信的协议状态 | 两端的通信组件 |
| 远端工作区 | 根目录、shell、执行记录 | 设备端进程 |
| 执行任务 | 某条命令的进度和输出 | 设备端工作区 |

MCP 的 HTTP 请求、协议会话和产品里的聊天窗口不是同一个对象。协议允许会话管理，也明确指出断开连接本身不应被当作取消请求。具体工作区如何存活，仍由 wanctl 自己实现。[原始资料：传输与会话管理](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports#session-management)

## 先看拥有状态的进程

当前目录和已导出的环境变量属于一个活着的 shell。把下一条命令交给同一个 shell，就能延续它们。启动同一种 shell 的新进程，得到的仍是另一份状态；子进程中的环境修改也不会自动改回父进程。[原始资料：Bash 执行环境](https://www.gnu.org/software/bash/manual/html_node/Command-Execution-Environment.html)

这也解释了 Python 的边界：shell 可以反复启动 Python 完成复杂逻辑，但每次新启动的 Python 程序不会自动继承上一次 Python 进程的内存变量。持久 Python 内核是另一种需要管理的进程，不是“支持执行 Python”自然附带的能力。[原始资料：Python subprocess](https://docs.python.org/3/library/subprocess.html#subprocess.run)

## 用四种事件检查寿命

<div class="course-lifetime">
<p><strong>概念演示：</strong>选择一种事件，观察工作区状态。这里不会连接设备。</p>
<div class="course-choices">
<button type="button" data-event="disconnect">网络断开</button>
<button type="button" data-event="controller">控制端重启</button>
<button type="button" data-event="agent">设备端重启</button>
<button type="button" data-event="exit">显式退出</button>
</div>
<p class="course-event-result" role="status" aria-live="polite">先问“谁持有这份状态”，再判断能否恢复。</p>
</div>

| 事件 | v0.12.1 的判断 |
| --- | --- |
| 网络断开 | 设备仍存活时，已受理任务可继续；重连后查原任务 |
| 保存绑定的控制端进程重启 | 远端状态可能还在，但调用方需要保存的引用来重新附着 |
| 设备端 agent 重启 | 进程内的 shell 和执行账本丢失，不自动复原；项目文件不因此自动删除 |
| 显式 exit | 工作区关闭，旧引用不能继续用于工作 |

这里的“持久”指跨调用和跨连接，不是任意进程重启后的内存快照恢复。[项目契约](https://github.com/Daily-AC/wanctl/blob/v0.12.1/docs/workspaces.md#reconnect-cancel-and-exit)

## 自检：先判断，再看解释

<div class="course-check" data-correct="1">
<p class="course-question">连接断开后，设备进程仍正常运行。原 shell 状态能否延续，主要取决于谁还活着？</p>
<div class="course-choices">
<button type="button" data-choice="0" aria-pressed="false">调用方进程</button>
<button type="button" data-choice="1" aria-pressed="false">设备端进程</button>
<button type="button" data-choice="2" aria-pressed="false">网页的标签</button>
</div>
<p class="course-feedback" role="status" aria-live="polite"></p>
<details><summary>查看解释（可以跳过作答）</summary><p>状态放在设备端工作区及其 shell 中。调用方仍需要有效授权和原引用才能找回它，但网页标签或网络连接的消失不等于远端进程退出。</p></details>
</div>

## 带着什么进入下一篇

以后遇到“重启后还在吗”，先画出状态的持有者。下一篇把这些状态放进一次完整的修改流程。

如果这一点还不清楚，可以把本篇的问题和你自己的项目场景交给协作 agent，请它换一个例子解释。自检只提供即时反馈，不代表已经掌握；隔一段时间，不看答案再解释一次更有价值。

[术语速查](reference/terms.md) · [架构卡片](reference/architecture-card.md) · [课程目录](../../portal/learning__remote-workspace.md)
