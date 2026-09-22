# 01｜全景：一次远程修改，谁负责哪一段

<div class="course-goal"><strong>这一篇的收获</strong><p>能把一次“修改远端代码并测试”的请求，分配给正确的组件。先辨认职责，不背进程名。</p></div>

## 从一个具体任务开始

你让 AI 修复远端项目的测试。你希望它读到的文件、保存的修改和执行测试的环境都属于同一个项目。聊天窗口在哪里，不应改变工作的地点。

先把系统放在三块区域里看。图中每个方框是一种职责，不一定是单独部署的服务。

<figure class="course-map"><div class="course-lanes"><section><strong>调用方</strong><p>宿主保存对话、组织工具调用。模型根据结果决定下一步。</p><p>wanctl 控制端选择设备、携带身份和工作区引用。</p></section><section><strong>到达设备</strong><p>Relay 转发连接数据，让控制端能够到达受控设备。</p><p>转发成功不等于命令已经执行。</p></section><section><strong>真正工作</strong><p>设备端检查权限，管理工作区和 shell，访问项目文件。</p><p>工作区保存这一段工作的运行状态。</p></section></div><figcaption>请求经过这些职责到达设备，执行结果沿调用链返回。项目文件和执行环境位于设备一侧。</figcaption></figure>

## 给每一段责任找主人

| 问题 | 主要由谁回答 |
| --- | --- |
| 下一步应该改什么 | 模型依据用户目标与工具结果判断 |
| 本次调用是否需要用户确认 | 宿主按自己的审批机制处理 |
| 请求发往哪台设备、哪个工作区 | 调用方与 wanctl 控制端保存并传递引用 |
| 能否到达设备 | 控制端、Relay 和设备连接共同完成 |
| 设备是否接受操作 | 设备端核对身份与原有操作策略 |
| 目录、变量和任务状态放在哪里 | 设备端工作区及其 shell |

MCP 的标准架构也区分宿主、客户端和服务端：宿主协调模型与连接，服务端提供能力。这里把 wanctl 的控制和设备职责继续展开，方便定位问题。[原始资料：MCP 架构](https://modelcontextprotocol.io/specification/2025-06-18/architecture)

## 用一个反例检查这张图

假如 AI 通过 wanctl 修改了远端的 `report.py`，随后却用宿主自带的本地终端运行测试，测试可能完全没有覆盖刚才的修改。网络没有报错，每个工具甚至都返回成功，但工作地点已经分叉。

所以第一个架构目标是：**读、改、运行必须落在同一次远端工作里。** 先证明这件事，再讨论连接复用或调用速度。wanctl 把项目根目录、shell 和执行记录放在设备端工作区中；这是项目自身的设计选择。[代码基线：设备端工作区](https://github.com/Daily-AC/wanctl/blob/v0.12.1/internal/agent/workspace.go)

## 这一层先不展开什么

现在不用研究 TLS 报文、进程组或 Go 的锁。它们分别服务于连接保护、进程管理和并发访问；等到对应职责出现具体问题，再向下一层追问。

## 自检：先判断，再看解释

<div class="course-check" data-correct="2">
<p class="course-question">网络已连通，但远端命令仍被设备策略拒绝。首先应检查哪一侧的操作授权？</p>
<div class="course-choices">
<button type="button" data-choice="0" aria-pressed="false">模型端</button>
<button type="button" data-choice="1" aria-pressed="false">中继端</button>
<button type="button" data-choice="2" aria-pressed="false">设备端</button>
</div>
<p class="course-feedback" role="status" aria-live="polite"></p>
<details><summary>查看解释（可以跳过作答）</summary><p>应先看设备端的拒绝及操作策略。Relay 能转发连接，不代表设备已批准执行；换模型或重复拨号并不能替代授权。</p></details>
</div>

## 带着什么进入下一篇

请先用自己的话说明：模型负责决定，设备负责执行，谁负责让两次调用继续落在同一个工作区？下一篇讨论工具入口能覆盖多大范围。

如果这一点还不清楚，可以把本篇的问题和你自己的项目场景交给协作 agent，请它换一个例子解释。自检只提供即时反馈，不代表已经掌握；隔一段时间，不看答案再解释一次更有价值。

[术语速查](reference/terms.md) · [架构卡片](reference/architecture-card.md) · [课程目录](../../portal/learning__remote-workspace.md)
