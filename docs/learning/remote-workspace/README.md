# 用 BFS 理解 wanctl 远程工作区

课程面向维护 wanctl 或接入 AI 宿主的开发者，沿用系统宏观架构、广度优先的阅读方式。课程目标见 [MISSION.md](MISSION.md)，原始资料见 [RESOURCES.md](RESOURCES.md)。

正式阅读入口：[wanctl Docs 架构课程](https://wc.z10.dev/docs/remote-workspace-course/)。仓库内仍可按以下顺序阅读正文：

1. [全景：一次远程修改，谁负责哪一段](01-system-map.md)
2. [边界：接上 MCP，哪些工具会去远端](02-integration-boundaries.md)
3. [状态：断掉一条连接，会丢掉什么](03-state-and-lifetime.md)
4. [流程：每一步要把什么交给下一步](04-workflows.md)
5. [故障与权限：结果不明时，先做什么](05-failures-and-authority.md)
6. [交付：什么证据才说明这套架构可用](06-delivery-and-evidence.md)
7. [接入：CLI、MCP 与 OAuth 各保存什么](07-cli-mcp-oauth.md)

每篇包含一个学习目标、架构图、具体判断和可跳过的自检。自检只给即时反馈，不记录个人成绩。另有 [术语速查](reference/terms.md) 和 [架构卡片](reference/architecture-card.md)。开发过程与历史验收仍保存在 docs/plans，课程正文不再承担发布流水账。

生成 Docs 和可独立打开、打印的教学 HTML：

```sh
uv run tools/docsite/build.py --course-export docs/learning/remote-workspace
```

生成的 lessons/*.html、reference/*.html 和 assets/site/ 不进版本控制。正文、课程清单与共享组件是维护入口。
