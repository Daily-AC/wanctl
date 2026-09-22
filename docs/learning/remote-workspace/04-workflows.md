# 04｜流程：每一步要把什么交给下一步

<div class="course-goal"><strong>这一篇的收获</strong><p>能读懂一次开发任务的结果交接：工作区引用、文件摘要、请求编号和输出偏移各有用途。</p></div>

## 用一次修复贯穿所有工具

模型要修改项目中的报表程序并运行测试。与其背工具名，不如看每一步必须留下什么证据。

| 当前动作 | 需要交给下一步的东西 | 下一步为什么需要 |
| --- | --- | --- |
| 进入工作区 | 工作区引用与根目录 | 让后续操作落到同一工作地点 |
| 读取文件 | 文件内容与整个文件的 SHA-256 | 确认改的是刚才看到的版本 |
| 修改文件 | 修改后的摘要与大小 | 核对结果，继续后续修改 |
| 提交命令 | 命令请求编号与当前状态 | 找回同一次任务，避免重复启动 |
| 轮询输出 | 新输出与下一段偏移 | 连续读取结果，判断是否完成 |
| 退出工作区 | 工作区的关闭状态 | 结束本次工作，不再使用旧引用 |

<figure class="course-map"><div class="course-lanes"><section><strong>定位</strong><p>工作区引用回答“在哪里做”。</p></section><section><strong>核对</strong><p>文件摘要回答“是否还是我读过的版本”。</p></section><section><strong>续接</strong><p>请求编号与偏移回答“哪次任务，读到了哪里”。</p></section></div><figcaption>这些字段解决不同问题，不能互相替代。</figcaption></figure>

## 项目根目录与 shell 当前目录

假设工作区根目录是 `/srv/app`，shell 已经切到 `src`。文件工具读取 `README.md`，仍按根目录解释为 `/srv/app/README.md`；shell 命令中的相对路径则跟随 shell 当前目录。分开这两个概念，可以避免一次 `cd` 悄悄改变后续文件工具的对象。[工作区路径契约](https://github.com/Daily-AC/wanctl/blob/v0.12.1/docs/workspaces.md#enter-and-work)

## 结构化结果把交接写清楚

Input Schema 描述传入参数，Output Schema 描述 `structuredContent` 的返回结构。Schema 给字段命名和类型，让调用方能校验结果；它不负责授予执行权限。[原始资料：MCP 输出结构](https://modelcontextprotocol.io/specification/2025-06-18/server/tools#output-schema)

v0.12.1 为八个核心工具提供输出声明。最值得先分清的是以下三项，而不是记住全部字段。

| 字段 | 表示什么 | 容易误读的地方 |
| --- | --- | --- |
| `state` | 工作区状态 | 命令完成后，工作区仍可保持 open |
| `done` | 所查询命令是否完成 | 生命周期回复没有命令时可以是 false |
| `code` | 命令完成后的退出码 | 无命令或未完成时不能当最终结论 |

例如 `exit` 成功时可能返回 `state=closed`、`done=false`、`code=0`。这里应以关闭状态判断生命周期，不应为了 `done=false` 再轮询一个不存在的命令。命令完成后也可能还有已保留的输出没读完：要继续到 `next_offset` 覆盖 `retained_bytes`。[本版输出契约](https://github.com/Daily-AC/wanctl/blob/v0.12.1/docs/mcp-output.md)

## 自检：先判断，再看解释

<div class="course-check" data-correct="0">
<p class="course-question">exit 返回 state=closed、done=false，且没有 request_id。应把它理解成什么？</p>
<div class="course-choices">
<button type="button" data-choice="0" aria-pressed="false">工作区已关闭</button>
<button type="button" data-choice="1" aria-pressed="false">命令仍在运行</button>
<button type="button" data-choice="2" aria-pressed="false">需要重新提交</button>
</div>
<p class="course-feedback" role="status" aria-live="polite"></p>
<details><summary>查看解释（可以跳过作答）</summary><p>state 描述工作区；done 描述被查询的命令。这个回复没有命令请求编号，done=false 不是关闭失败，也不构成重新提交任务的理由。</p></details>
</div>

## 带着什么进入下一篇

能够说明“这一步的返回值供哪一步使用”，就读懂了主流程。下一篇看结果没有回来或操作被拒绝时，该如何保持这个流程可信。

如果这一点还不清楚，可以把本篇的问题和你自己的项目场景交给协作 agent，请它换一个例子解释。自检只提供即时反馈，不代表已经掌握；隔一段时间，不看答案再解释一次更有价值。

[术语速查](reference/terms.md) · [架构卡片](reference/architecture-card.md) · [课程目录](../../portal/learning__remote-workspace.md)
