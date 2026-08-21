控制端（你自己的电脑）同样先设好两个地址（写进 shell 配置文件）：

```
export WANCTL_RELAY=https://relay.example.com
export WANCTL_PORTAL=https://portal.example.com
```

`wanctl login` 登录一次拿到身份（浏览器门户登录 → 一次性 code 贴回终端）。然后直接用：

```
wanctl peers                                  # 看有哪些设备在线
wanctl exec --target 设备名 "uname -a"        # 在它上面跑命令
wanctl push --target 设备名 ./本地 /远程       # 传文件上去
wanctl pull --target 设备名 /远程 ./本地       # 拉文件下来
```

第一次控制某台新设备，会在那台设备的网页「待审批 / 已信任」里等它点头。控制好友的设备要先拿到共享授权（见「邀请、好友与共享」）。
