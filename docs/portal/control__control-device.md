控制端（你自己的电脑）装好工具后，先把实例地址配置好（一次即可，`wanctl config` 随时查看/修改）：

```
wanctl config set relay=https://relay.example.com portal=https://portal.example.com
```

`wanctl login` 登录一次拿到身份（浏览器门户登录 → 一次性 code 贴回终端）。然后直接用：

```
wanctl peers                                  # which devices are online
wanctl exec --target DEVICE "uname -a"        # run a command on it
wanctl push --target DEVICE ./local /remote   # send a file up
wanctl pull --target DEVICE /remote ./local   # pull a file down
```

设备主人可以给设备起一个别名：在门户里打开这台设备，点标题行的齿轮，「别名」那一栏填好保存；清空再保存就是取消。别名在你的命名空间内唯一，不能和某台设备的真实名字重名。

设好之后，设备列表和设备页用别名当显示名，真实设备名仍在旁边——别名只是更好记的叫法，不会把机器的身份藏起来。`wanctl peers` 会在真实设备名后的括号里显示别名；上述命令的 `--target` 既可以填真实设备名，也可以直接填别名。控制共享设备时同样可写成 `主人namespace/别名`。

`wanctl peers` 除了你自己的在线设备，还会单列一段「shared with you」，列出别人共享给你的设备，写成 `主人namespace/设备`——直接整条粘进 `--target` 就能用。不带 namespace 的名字先在你自己的 namespace 里找，找不到才去共享设备里找；共享设备的名字在两个 namespace 里可能重名，重名时会直接报错，让你改用完整写法。

第一次控制某台新设备，会在那台设备的网页「待审批 / 已信任」里等它点头。控制好友的设备要先拿到共享授权；想自己批这台设备的审批，还要那条共享带上管理权（见「邀请、好友与共享」）。

## 在设备页升级

只有设备主人能看到版本旁的按钮。wanctl 的**升级**会打开版本输入框，可填写严格比
当前版本新的正式版（如 `v0.27.0`）或 beta（如 `v0.27.0-beta.1`）。只有最新正式版
比当前版本新时才会预填。不允许安装相同版本或降级。即使设备的 `auto_update` 已关闭，
主人的这次主动升级仍会执行。

Windows 设备上，**安装**会为当前已登录的控制台用户安装 OpenCU；**更新到最新版**
会更新该用户已有的安装。请先在设备上登录，并结束正在进行的桌面或电脑操作会话。
wanctl 会关闭自己闲置的 OpenCU 连接；若仍有其他 OpenCU 进程在运行，官方安装器会
拒绝更新。OpenCU 与手动安装使用相同的信任来源：到 github.com 的 HTTPS，以及
官方安装脚本内嵌的压缩包 SHA-256。

设备离线时按钮不可用。点击后会显示下载、安装、完成或失败原因；wanctl 只有在设备
重新上线并报告目标版本后才显示完成。失败或十分钟未完成时，可在设备上运行
`wanctl logs --type install` 查看日志。开发版、Android 应用内置副本、不可写的二进制
目录以及独立的 Android/Termux 安装会像自动更新一样被拒绝，页面会说明处理方法；
Android 应用请使用应用内的更新检查。其他启动方式沿用自动更新的重启路径：Windows 的
`--managed` 依赖外部循环重启，包括 SYSTEM PowerShell 脚本和现有的计划任务 supervisor；
使用升级按钮前无需更新或重启父进程，也不需要新的父进程标记。
找不到版本或连不上下载源时，页面用一句话说明；完整下载错误保存在设备安装日志中。
旧 agent 会提示等待六小时内的自动更新，或在设备上运行 `wanctl update`。
进度只临时保存在门户内存中，门户重启后会消失。
