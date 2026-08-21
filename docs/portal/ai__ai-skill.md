让 AI（如 Claude Code）当"控制端"，你在网页审批。两步：

**a.** 跟你的 AI 说一句：

> 安装 https://relay.example.com/skills

AI 会 fetch 这个 URL 拿到 wanctl skill 写到本机 `~/.claude/skills/wanctl/SKILL.md`，重启它一下 skill 就生效了。

> 一定要给 AI **relay 域**（`relay.example.com`）而不是门户域。门户域要走浏览器登录，AI 直接 fetch 会被登录页挡住，拿不到 skill 正文。

**b.** 让 AI 跑一次 `wanctl login`：会弹个浏览器让你在门户登录（GitHub 账号）拿一次性 code，复制回去给 AI 回车即可——AI 这台机器就有你的身份了，可以用 `wanctl exec/push/pull` 控制你授权的设备。

> AI 第一次连某台设备 → 那台设备的网页「待审批」会冒出**配对请求**，写着 AI 自报的名字，你点「信任它」即可。之后它发的每条命令仍会在「待审批」等你点头（除非你切到「放飞」模式或在「规则」里预放行）。
