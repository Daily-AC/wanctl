# 02｜边界：接上 MCP，哪些工具会去远端

<div class="course-goal"><strong>这一篇的收获</strong><p>能判断某项操作是否经过 wanctl，以及“整个宿主切换到远端”还需要谁参与。</p></div>

## 连接成功以后，先问路径

MCP 让宿主发现工具、提交参数并接收结果。它建立的是工具调用通道。wanctl 可以在这条通道里提供文件读写、命令执行和工作区管理。[原始资料：MCP 工具](https://modelcontextprotocol.io/specification/2025-06-18/server/tools)

但宿主可能同时拥有自己的本地文件工具、浏览器和终端。只有真正走到 wanctl 的调用，才受 wanctl 的工作区绑定影响。

<figure class="course-map"><div class="course-lanes two"><section><strong>wanctl 工具</strong><p>读取、编辑、执行等请求携带工作区引用，交给目标设备处理。</p></section><section><strong>宿主的其他工具</strong><p>仍按宿主自己的实现选择地点。wanctl 无法拦截一个没有经过它的本地文件调用。</p></section></div><figcaption>先确认调用经过哪个入口，才能判断它工作的地点。</figcaption></figure>

## 三种接入程度

| 接入方式 | 工作区绑定放在哪里 | 可以得到的体验 |
| --- | --- | --- |
| 通用 MCP | 调用方保存引用，每次显式携带 | 多个聊天可各自使用独立工作区 |
| 专属 stdio MCP | 独占一个对话的 MCP 进程保存绑定 | wanctl 工具可以省去重复填写引用 |
| 宿主工作区后端 | 宿主为对话统一选择文件和执行后端 | 宿主原有工具也能一起切换工作地点 |

v0.12.1 已提供前两种。第三种需要具体宿主的适配，不能仅靠安装 MCP 自动获得。专属模式的前提是“一段对话独占该 MCP 进程”；共享 HTTP 服务不能把所有聊天当作一个对话。[实现与用法](https://github.com/Daily-AC/wanctl/blob/v0.12.1/docs/workspaces.md)

## 为什么会想到 VS Code Remote

VS Code 把界面相关扩展和工作区相关扩展分开安排：后者可以靠近远端项目运行。值得借鉴的是“界面留在眼前，工作能力靠近文件和运行环境”的分工。[原始资料：VS Code 远程扩展架构](https://code.visualstudio.com/api/advanced-topics/remote-extensions#architecture-and-extension-kinds)

在 wanctl 中，通用 MCP 已经能统一它自己的远端工具；若要让宿主所有原生工具都跟着切换，需要宿主也保存并使用这个工作地点。评审设计时，应明确自己正在承诺哪一种接入程度。

## 自检：先判断，再看解释

<div class="course-check" data-correct="1">
<p class="course-question">AI 已进入 wanctl 工作区，却调用了宿主原生的本地读文件工具。谁能决定这次本地读取是否改走远端？</p>
<div class="course-choices">
<button type="button" data-choice="0" aria-pressed="false">模型推理层</button>
<button type="button" data-choice="1" aria-pressed="false">宿主接入层</button>
<button type="button" data-choice="2" aria-pressed="false">设备执行层</button>
</div>
<p class="course-feedback" role="status" aria-live="polite"></p>
<details><summary>查看解释（可以跳过作答）</summary><p>要改变原生工具的路由，必须由宿主的接入或后端适配层参与。模型可以选择正确工具，但 wanctl 不能拦截没有经过它的调用。</p></details>
</div>

## 带着什么进入下一篇

看到“支持 MCP”时，再补问一句：覆盖的是哪组工具？下一篇讨论这些工具依赖的状态各自活多久。

如果这一点还不清楚，可以把本篇的问题和你自己的项目场景交给协作 agent，请它换一个例子解释。自检只提供即时反馈，不代表已经掌握；隔一段时间，不看答案再解释一次更有价值。

[术语速查](reference/terms.md) · [架构卡片](reference/architecture-card.md) · [课程目录](../../portal/learning__remote-workspace.md)
