这套课程面向参与 wanctl 开发、评审或 AI 宿主接入的人。目标是能解释一个远程工作区如何成立，并据此判断设计和验收；不需要先掌握 Go、进程组或网络报文。

## 按广度优先阅读

先看完整系统，再看边界和状态，最后看流程、故障、证据与接入选择。每篇只解决一个架构判断，包含一张图、具体例子和可跳过的自检。

| 顺序 | 课程 | 读完能回答 |
| --- | --- | --- |
| 01 | [系统全景](https://wc.z10.dev/docs/learn-system-map/) | 一次远程修改由谁完成哪一段 |
| 02 | [接入边界](https://wc.z10.dev/docs/learn-integration-boundaries/) | MCP 能覆盖哪些工具，哪里需要宿主适配 |
| 03 | [状态与寿命](https://wc.z10.dev/docs/learn-state-and-lifetime/) | 连接断开和进程重启分别丢掉什么 |
| 04 | [正常工作流](https://wc.z10.dev/docs/learn-workflows/) | 引用、摘要、请求编号和偏移怎样接续操作 |
| 05 | [故障与权限](https://wc.z10.dev/docs/learn-failures-and-authority/) | 结果未知或请求被拒绝后，应该先做什么 |
| 06 | [交付与证据](https://wc.z10.dev/docs/learn-delivery-and-evidence/) | 一份测试结果究竟证明了哪一项能力 |
| 07 | [CLI、MCP 与 OAuth](https://wc.z10.dev/docs/learn-cli-mcp-oauth/) | 不同宿主应怎样保存身份授权与工作区绑定 |

## 随时回来查

- [术语速查](https://wc.z10.dev/docs/learn-terms/)：统一“宿主、控制端、工作区、授权、请求编号”等用语。
- [可打印的架构卡片](https://wc.z10.dev/docs/learn-architecture-card/)：用六个问题复习和评审方案。
- [完整使用契约](https://github.com/Daily-AC/wanctl/blob/v0.12.1/docs/workspaces.md)：需要具体参数、限制和操作时再查。
- [原始资料索引](https://github.com/Daily-AC/wanctl/blob/main/docs/learning/remote-workspace/RESOURCES.md)：区分协议事实和项目设计，继续深入。

## 怎样使用这套课程

正文目前为中文，代码基线是 v0.12.1。课堂图示是宏观模型，已实现的能力、边界和扩展方向会明确区分。网页版的判断题只提供本页反馈，不上传答案，也不记录个人成绩。

先试着不用术语解释一遍，再对照图和原始资料。拿不准时，把具体场景交给协作 agent，让它沿同一层架构解释；不必马上钻进源码。阅读与答题都不是准入门槛。
