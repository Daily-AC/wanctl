# 06｜交付：什么证据才说明这套架构可用

<div class="course-goal"><strong>这一篇的收获</strong><p>能把“看起来连上了”拆成可核验的结论，并指出一份验收报告还没有证明什么。</p></div>

## 先写清楚要证明的句子

“支持 MCP”“测试全绿”“速度更快”都太宽泛。架构验收应把承诺落到一个具体观察上。

| 想证明的能力 | 最小可核验证据 | 还不能由此推出 |
| --- | --- | --- |
| 工具可被发现 | tools/list 中能读到名称与 Schema | 工具已经能访问设备 |
| 可以真实工作 | 在目标项目读、改、运行测试 | 断网后仍能继续 |
| shell 状态持续 | 分开的调用设置再读取目录／变量 | agent 重启后内存还在 |
| 工作区独立 | 两个工作区的 shell 状态不相互覆盖 | 共享项目文件不会互相影响 |
| 请求去重 | 相同编号重发后，副作用只发生一次 | 任意重启后仍恰好执行一次 |
| 能正确退出 | 关闭回执与旧引用拒绝 | 所有宿主审批都会自动放行 |

## 用同一个小任务逐层验证

选择一个可以独立检查产物的小项目，例如 CSV 汇总程序。先证明文件确实在目标设备，接着跨调用保留一个环境变量，再提交测试并收齐输出。增加第二个工作区观察状态隔离，最后验证去重与退出。

<figure class="course-map"><div class="course-lanes"><section><strong>接口证据</strong><p>参数和结果是否符合契约？</p></section><section><strong>任务证据</strong><p>目标设备是否产生了正确文件和测试结果？</p></section><section><strong>使用证据</strong><p>真实模型能否持续完成任务，遇到中断能否恢复？</p></section></div><figcaption>三类证据回答不同问题；接口测试不能代替真实任务，单次任务也不能证明长期使用体验。</figcaption></figure>

在 v0.12.1 的代码基线中，HTTP 测试会为多次工具调用新建 MCP 会话，并使用真实 relay、设备 shell 和文件系统；stdio 模式也经过子进程验证。这验证了服务端契约，但仍不能替代特定网页宿主的审批与界面测试。[测试入口](https://github.com/Daily-AC/wanctl/blob/v0.12.1/internal/mcp/workspace_test.go) · [输出结构验证](https://github.com/Daily-AC/wanctl/blob/v0.12.1/internal/mcp/output_test.go)

## 怎样读“网页被拦住”的结果

如果宿主在工具调用之前拒绝请求，该轮就没有完成设备操作验收。它不能记成执行成功，也不能仅凭这个拒绝判断设备实现有错。应保留拒绝的位置与原始提示，遵循批准流程；没有具体原因时不要编造原因。[原始资料：MCP 的用户交互与审批](https://modelcontextprotocol.io/specification/2025-06-18/server/tools#user-interaction-model)

同样，交叉编译只证明能构建那个平台的程序，不能写成真机使用通过。v0.12.0 的公开验收记录保留了开发任务与宿主摩擦的边界，适合对照阅读；它是当时的证据，不是所有未来版本的保证。[验收记录](https://github.com/Daily-AC/wanctl/blob/v0.12.1/docs/plans/2026-09-22-web-workspace-completion.md)

## 什么时候再加新能力

先观察用户是否经常被同一问题打断。若需要交互式输入，再讨论 PTY；若确实需要长期保留 Python 对象，再讨论 Python 内核；若远端服务必须在本地浏览器预览，再讨论端口转发。每加一种状态，就要补充它的归属、寿命和失败处理。

## 自检：先判断，再看解释

<div class="course-check" data-correct="0">
<p class="course-question">报告只写“全部平台交叉编译通过”。它已经证明了哪一项？</p>
<div class="course-choices">
<button type="button" data-choice="0" aria-pressed="false">可以构建程序</button>
<button type="button" data-choice="1" aria-pressed="false">真机操作通过</button>
<button type="button" data-choice="2" aria-pressed="false">网页流程顺畅</button>
</div>
<p class="course-feedback" role="status" aria-live="polite"></p>
<details><summary>查看解释（可以跳过作答）</summary><p>交叉编译验证构建链路和平台相关代码是否可编译。设备行为、用户权限、真实网络和宿主交互仍需要对应环境的验收。</p></details>
</div>

## 带着什么进入下一篇

评审时，可以追问“这个证据支持哪一句承诺”。最后一篇把前面的判断落到 CLI、专属 MCP 与网页 MCP 的接入选择。

如果这一点还不清楚，可以把本篇的问题和你自己的项目场景交给协作 agent，请它换一个例子解释。自检只提供即时反馈，不代表已经掌握；隔一段时间，不看答案再解释一次更有价值。

[术语速查](reference/terms.md) · [架构卡片](reference/architecture-card.md) · [课程目录](../../portal/learning__remote-workspace.md)
