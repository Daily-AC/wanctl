控制端（你自己的电脑）装好工具后，先把实例地址配置好（一次即可，`wanctl config` 随时查看/修改）：

```
wanctl config set relay=https://relay.example.com portal=https://portal.example.com
```

`wanctl login` 登录一次拿到身份（浏览器门户登录 → 一次性 code 贴回终端）。然后直接用：

```
wanctl peers                                  # 看有哪些设备在线
wanctl exec --target 设备名 "uname -a"        # 在它上面跑命令
wanctl push --target 设备名 ./本地 /远程       # 传文件上去
wanctl pull --target 设备名 /远程 ./本地       # 拉文件下来
```

设备主人可以在 portal 给设备设置别名。`wanctl peers` 会在真实设备名后显示别名；上述命令的 `--target` 既可以填写真实设备名，也可以直接填写别名。控制共享设备时同样可写成 `主人 namespace/别名`。

第一次控制某台新设备，会在那台设备的网页「待审批 / 已信任」里等它点头。控制好友的设备要先拿到共享授权（见「邀请、好友与共享」）。
