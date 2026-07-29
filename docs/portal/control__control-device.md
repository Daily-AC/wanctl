同样先 `wanctl login` 登录一次（拿到身份）。然后零配置直接用：

```
wanctl peers                                  # 看有哪些设备在线
wanctl exec --target 设备名 "uname -a"        # 在它上面跑命令
wanctl push --target 设备名 ./本地 /远程       # 传文件上去
wanctl pull --target 设备名 /远程 ./本地       # 拉文件下来
```

relay 地址、传输方式都内置了默认值，**不用配任何环境变量**。第一次控制某台新设备，会在那台设备的网页「待审批 / 已信任」里等它点头。