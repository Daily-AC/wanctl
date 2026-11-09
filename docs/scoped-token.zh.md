# 限权令牌（scoped token）

限权令牌是给「只需要从设备上拿一件事」的程序用的凭据，比如「在 A 机上重启应用」
「跑夜间备份」。令牌泄露或程序被诱导时，它也做不了别的。

它只能在**一台设备上运行一条固定命令**。其余一律拒绝：换命令、同一条命令改参数、
末尾加管道或 `;`、换设备、文件、日志、shell、你的账号、其他令牌。它会过期，你随时可以撤销。

> **需要 v0.30.0。** relay 和设备都要是 wanctl v0.30.0 或更新。relay 版本更旧时，
> `wanctl scoped-token` 和它发出的令牌都会被拒绝；设备版本更旧时，设备会拒绝这些令牌。

## 发一把

用平常的登录（`wanctl login`），在任意一台机器上：

```bash
wanctl scoped-token create --device home-pc --command 'systemctl restart app' --days 30 --label ci
```

令牌（`wcs_…`）只在 stdout 打印一次，说明写在 stderr，所以
`T=$(wanctl scoped-token create …)` 拿到的只有令牌。丢了不能再看，只能重发。

- `--device` 必须是你自己的设备，不能是别人共享给你的。
- `--command` 按字节逐一比对。一行可见字符，最长 4096 字节。更长的写成设备上的脚本，
  让令牌只允许运行这个脚本。
- `--days` 取 1 到 365，默认 90。没有永不过期的限权令牌。
- 同时有效的限权令牌最多 50 把。

```bash
wanctl scoped-token list          # id, state, command, device, label
wanctl scoped-token revoke 12     # its next request is refused
```

## 使用

把令牌交给程序，设为 `WANCTL_TOKEN`，加 `--oneshot` 运行这条命令：

```bash
WANCTL_TOKEN=wcs_… wanctl exec --target home-pc --oneshot -- 'systemctl restart app'
```

用令牌的那台机器和任何控制端一样，要先确认一次设备身份：
`wanctl trust server --target home-pc --fingerprint SHA256:…`，指纹是设备上 agent 启动时打印的那一个。

除了这一条请求，别的都会被 relay 或设备拒绝，什么都不会执行。撤销或过期的令牌在下一次请求时失效，
已经打开的会话也一样。

## 它不做什么

限权令牌只会**收窄**权限，不会增加权限。

- 设备自己的模式、规则和审批仍然决定命令跑不跑。设备在「每次询问」模式时，要么用规则
  （`wanctl rules`）一次性放行这条命令，要么调用会像别的请求一样等你批准。
- 用令牌的那台机器仍然要和设备配对，和任何控制端一样。令牌被偷、从设备没见过的机器上使用，
  碰到的是设备的配对询问，不是命令执行。
- 设备上的 wanctl 必须是支持限权令牌的版本。旧版设备会拒绝这把令牌，不会忽略限制。
- 它不是保密工具：拿到令牌的人能随时运行那条命令，所以要选可以随时跑的命令。

## 怎么执行的

会话是端到端加密的，relay 看不到命令，所以权限分两处判断。relay 只把令牌放行到它绑定的
那台设备，且只给执行权限，并通过设备自己的已认证通道告诉设备授权的是哪条命令。设备只接受
恰好是「运行这条命令一次」的请求，其余全部拒绝，包括它不认识的任何字段。改客户端没用：
规则在设备手里，relay 也在每次请求时重新检查令牌。设计取舍见
ADR 0021（仓库里的 `docs/adr/0021-scoped-command-token.md`）。
