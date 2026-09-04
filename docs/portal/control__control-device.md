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

第一次控制某台新设备，会在那台设备的网页「待审批 / 已信任」里等它点头。控制好友的设备要先拿到共享授权（见「邀请、好友与共享」）。
