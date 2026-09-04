# Android 设备

wanctl 在 Android 上是以**被控设备**的身份运行的——你从一个终端驱动一台 Android
手机或平板，方式跟驱动一台 Linux 机器一模一样。控制端那一侧（*在* Android 上跑
`wanctl exec`）不是另一个产品：同一个二进制里就带着控制端子命令，只是没有在那里被
认真跑过而已。

跑起来有两条路，它们是实实在在的两个产品：

| | APK（推荐） | Termux |
|---|---|---|
| 重启后还在 | 在，靠 `BOOT_COMPLETED` | 不在 |
| 关掉 app 后还在 | 在，前台服务 | `wanctl start` 会脱离，直到被 Android 杀掉 |
| `wanctl update` | 不行——更新以 APK 形式发 | 行 |
| 需要另装一个 app | 不需要 | 需要 Termux，从 F-Droid 装 |
| 依赖的绕行手法 | 没有 | 四个，全在 Termux 的内部机制上 |

发出去的产物：每个 ABI 一个 APK 加一个裸二进制——`wanctl-android-arm64`
（`arm64-v8a`，几乎所有手机和平板都是这个）、`wanctl-android-arm`
（`armeabi-v7a`：老手机、电视盒子、手表）、`wanctl-android-386`（`x86`）和
`wanctl-android-amd64`（`x86_64`：模拟器、Chromebook），每个都配一个对应的 `.apk`。
装跟你设备 ABI 对上的那个 APK；之后 app 会通过与它携带的二进制相匹配的 APK 自我更新。
arm64 那个二进制是静态的，另外三个是经 NDK 链接到 bionic 的，
因为 Go 对那几个目标没法用内部链接器。

## 这个 app

```sh
# from the relay, on the device itself
https://relay.example.com/dl/wanctl-android-arm64.apk
```

装上，打开，点**登录**，然后跟其它每个平台一样走一遍飞书接入。接着打开
**启用 wanctl**。agent 以前台服务运行——会有一条常驻通知，这就是 Android 给的交易：
系统让进程活着，用户则始终知道它在。

界面是五个开关加四个按钮：

- **加入电池优化白名单**是**不可选的**，不管文案听起来像什么。Android 只允许被豁免的
  app 在后台启动前台服务；重启之后那次成功的启动，日志里白纸黑字写着
  `am_foreground_service_start: … SYSTEM_ALLOW_LISTED`。没有这个豁免，
  agent 就不会自己回来。这个服务还额外持有一个 partial wake lock，
  也就是 Termux 那条路上 `termux-wake-lock` 干的事。
  **在国产 OEM ROM 上这只做到一半——看下一节，那才是一台能用的设备和一台不能用的设备
  之间的区别。**
- **开机自启**（默认开）在重启之后、以及 app 自我更新之后重新拉起 agent。
  它保证什么、不保证什么，见下面「重启之后怎么回来」。
- **自动信任新控制端**默认是**关**的，也应该一直关着。关着的时候，
  一个未知控制端的配对请求会被抬到门户的网页控制台上等人做决定，
  这在一台没有键盘的设备上是行得通的。开着的话，任何握着命名空间令牌的东西都能静默配对。
- **自动放行所有命令**默认是**关**的，把它打开是一个真正的决定。关着时，
  agent 跑在 wanctl 的 `normal` 策略模式下：一条没有匹配任何规则的命令会被拒绝，
  直到有人在门户控制台里批准它。这对一台有人看着的设备是对的，对一台无人值守的设备
  则没法用——而一个 APK 没有 shell 给你敲 `wanctl rules`，
  所以这个开关是唯一能表达这件事的地方。在你做出选择之前，
  预期会一直看到 `command denied by device policy`。
- **设备名**默认取机型名（`pa2353`）。两台同型号的设备会撞名，在这里给其中一台改掉。

### OEM 的那几道闸，它们决定这一切到底成不成立

在这里，Android 自己的那些控制项反而不是最要紧的。国产 OEM ROM 上还有两道，
两道对 app 都是不可见的，而第二道就是一台能用的设备和一台没用的设备之间的差别。

**自启动**决定 `BOOT_COMPLETED` 到底会不会被送达。在 PA2353 上 wanctl 是自动被授予的；
别指望这一点。如果 agent 重启之后再也不回来，去查 设置 → 应用与权限 → 自启动。

**后台耗电管理**决定 app 在息屏时会不会被*冻结*——而它的默认值就是冻结。
在 PA2353 上实测，电池优化豁免已授予、自启动已开、前台服务在跑、partial wake lock
也握着：

| | freezer cgroup | 进程状态 | 是否可达 |
|---|---|---|---|
| 智能控制后台耗电（默认） | `freezer:/frozen` | `D`，全部 11 个线程 | 不可达，息屏约 2 分钟内 |
| 允许后台高耗电 | `freezer:/` | `S` | 可达，8 分钟还在继续 |

被冻结意味着轮询循环根本没在跑——没有错误、没有重试、没有日志行，app 也拿它没辙。
前台服务防不住它。`PARTIAL_WAKE_LOCK` 防不住它。AOSP 的电池豁免也防不住它。

设置 → 电池 → 后台耗电管理 → wanctl → **允许后台高耗电**。
同一台设备上的 Termux 就是这么设的，这也是为什么 Termux 那条路的锁屏测试当初过了。
app 里的**电池**按钮会跳到 OPPO、小米和华为上的等价路径。

### 给一台 APK 设备配对，按顺序来

这几道闸是一道一道触发的，每一道的修法都不一样，所以头三次 `wanctl exec`
失败的方式各不相同：

1. **控制端必须确认设备的身份。** `wanctl exec` 会打印一行
   `wanctl trust server --target … --fingerprint …`。把那个指纹跟 app 里 指纹
   那一栏显示的比一比——这一比就是全部意义所在，所以请用眼睛比，别去粘贴。
2. **设备必须信任这个控制端。** 除非 自动信任新控制端 是开的，
   否则 agent 会拒绝，并打印一个五分钟内有效的门户 URL，给设备主人点。
3. **命令必须过策略。** 见上面的 自动放行所有命令。

### 文件落在哪里

`wanctl push` 和 `pull` 的目标必须是 *app* 写得进去的目录，
也就是 `/data/user/0/dev.wanctl.agent/` 底下的某个地方。`/data/local/tmp`
是 adb shell 用户写得进去的，app **写不进去**——往那里推会以 `permission denied`
失败，尽管同一个路径对一个 adb 推过去的二进制是好用的。
exec 会话从 app 自己的 files 目录开始，所以相对路径会落在一个说得通的地方。

### 内建的电池状态

APK 版的 agent 不用往 Android 那个受限的 app shell 里发命令，就能给出电池状态：

```sh
wanctl exec --target phone -- battery
```

这条命令往 stdout 写一个 JSON 对象：

```json
{"level":76,"status":"charging","plugged":"usb","temperature_c":31.4,"health":"good","updated_at":"2026-08-13T18:42:03Z","age_seconds":12}
```

`level` 是百分比。`status` 是 `charging`、`discharging`、`full`、`not charging`
或 `unknown`；`plugged` 是 `ac`、`usb`、`wireless`、`none` 或 `unknown`。
`temperature_c` 是摄氏度，`health` 是 Android 归一化后的电池健康度，
`updated_at` 是 app 里那个采集器的 UTC 时间戳。`age_seconds` 是 Go agent
回答时算出来的。

那个 Java 服务在启动时、以及每当 Android 发来电池变化广播时，
把源快照原子地写进 `files/state/device.json`。Go 子进程通过环境变量拿到这个文件的
绝对路径。快照缺失、损坏，或者超过十分钟没更新，都会被报成不可用，
而不是当成当前数据返回。这个动词只有在 agent 由 wanctl 的 APK 拉起时才可用；
其它平台会返回一个明确的「仅 Android」错误。动词跑之前，普通的 exec 策略照样生效。

### 提权：把 adb 剩下的那部分打通

上面这些全都发生在 app 沙箱里面，而 `adb` 的大部分表面从那里够不着。
`pm`、`am`、`input`、`screencap`、`dumpsys`、`settings`、`wm` 和 `svc` 需要 uid 2000
（`shell`）或者 uid 0；agent 是 `untrusted_app` 里一个普通的 app uid。
这不是 wanctl 的一个可以逐个动词绕过去的限制——这是平台的限制，
也正是为什么内建的 `battery` 动词不得不在 Java 里采集它的数据。

app 里的**提权通道**会开一条跨过那条线的通道。它默认关闭，
而且它跟这里其它每个开关都是分开的决定：

```sh
wanctl exec --target phone --elevate -- pm list packages -3
wanctl exec --target phone --elevate --via su -- dumpsys battery
```

一共存在三条通道；agent 按顺序探测，用第一条可用的。`--via` 钉住其中一条，
而钉住一条不可用的通道会直接报错，不会降级——一条在你要求 root 之后却以非特权身份
跑掉的命令，是一个穿着正确答案外衣的错误答案。

| 通道 | 需要 | 重启后还在 |
|---|---|---|
| `su` | 一台已 root 的设备 | **在** |
| `adb` | 开发者选项 → 无线调试 | 不在——Android 开机就把它清掉 |

只有 `su` 能在没人碰这台设备的情况下继续工作。一台断电之后必须保持无人值守可控的手机，
要么应该 root，要么就不该依赖提权。

第三条通道 Shizuku 原本在计划里，2026-08-14 砍掉了：它到达的是 `adb` 通道已经到达的那个
uid 2000，它自己也是靠无线调试启动的，而且它要求设备主人再装并重启第二个 app。
`--via shizuku` 会把这些说出来，而不是报一个未知通道。

#### 配对，给一台没 root 也没线的手机

Android 11+ 能不经过电脑、直接通过 Wi-Fi 发放 shell 访问权，而 agent 能接住它。
在设备上：设置 → 开发者选项 → **无线调试** → **使用配对码配对设备**。
那个界面会显示一个端口和六位数字。然后，从控制端：

```sh
wanctl exec --target phone -- adb-pair 37129 314159
```

这个动词跑**在设备上**——只有它够得着自家 adbd 打开的那个配对端口——
而且它不需要提权，因为「用一条本身需要提权的命令来建立提权通道」是一个没有入口的死循环。
它做的全部事情就是连 127.0.0.1，而这是一个 app 沙箱本来就可以做的。

那个界面上会出现两个端口，它们不是同一个监听器：用配对码旁边那个，
不是无线调试主界面上那个。这里出现 `connection refused`，几乎总是这个混淆。

底下发生了什么，因为它不显然：配对码本身并不是 SPAKE2 的口令。
AOSP 会在它后面追加 64 字节的 TLS 导出密钥材料，把这次交换绑定到那个具体的 TLS 会话上，
好让一次被转发的配对偷不走。wanctl 做的是同一件事——包括这个细节：
导出标签是 `"adb-label\0"`，**带着**它结尾的那个 NUL，因为 AOSP 传的标签长度是
`sizeof()`。少了那个字节，产出的密钥材料完全合法，只是永远对不上设备那边的。

配对是持久的：密钥落在 `/data/misc/adb/adb_keys` 里，能扛过重启。
无线调试本身则不能——Android 开机就把它关掉——所以重启之后那个开关得再打开一次，
但配对码不用再输一遍。

**连接**端口也不稳定，而且它不是配对端口：IP 地址和端口 底下那个数字，
在无线调试被重新打开时、以及重新配对之后都会变（在一台 PGBM10 上实测：37819 → 41031）。
app 不会问你要它——它盯着 mDNS 上的 `_adb-tls-connect._tcp`，
把找到的东西通过电池动词用的那同一个状态文件递给 agent。只有它自己这台设备的广播才算数：
同一个 Wi-Fi 上每一台开着无线调试的手机都会广播那个服务，
所以解析出来的地址会先跟本机自己的地址核对过，才被采信。

那个发现逻辑是 Java 写的（`NsdManager`），因为 mDNS 在 Android 上是一个框架服务，
而且 app 在盯着的时候会握住一个 `MulticastLock`——没人握的时候 Wi-Fi
会在硬件层把多播过滤掉。盯梢随**提权通道**开关一起启停：
一台主人没打开提权的设备，不会去听那个用来启用提权的端口。

`WANCTL_ADB_PORT` 仍然覆盖一切，这正是 Termux 和 adb-shell 两条路需要的：
它们没有框架可问。

#### adb 通道需要人一次，而且过不去这一关就没法自动化

agent 第一次连自家 adbd 的时候，设备会弹出**允许 USB 调试吗？**，
上面显示着 wanctl 的密钥指纹。在有人接受它之前，这条通道报的是：

```
elevation channel "adb" is not available: adbd on port 5555 is waiting for
someone to allow wanctl's key on the device screen
```

那个弹窗没法用软件回答。2026-08-14 在一台小米 10 至尊版（Android 11）上实测，
对它用 `input tap` 会失败于

```
java.lang.SecurityException: Injecting to another application requires INJECT_EVENTS permission
```

而这正是平台按设计工作的样子：如果一个程序能点那个按钮，
那任何一个程序都能给自己发 shell 权限。所以打开这条通道永远要付出一次由拿着手机的那个人
做出的、有意识的动作——在计划一次无人值守的铺开之前，值得先知道这一点。

还要注意 adbd 在等待期间做什么：**什么都不做**。它不重发 token，也不关 socket。
一个只是傻等的客户端看到的是一个光秃秃的 `i/o timeout`，
完全没有暗示有个对话框正开着——这就是为什么 agent 在一小段宽限期之后就放弃，
转而报告那个对话框。

**提权命令是它们自己的一个策略类别。** 自动放行所有命令 覆盖不到它们：
那个开关说的是「这台设备无人值守」，不是「把 root 发出去」，
所以在一台 bypass 模式的设备上，一条提权命令照样被拒绝，
直到它有了自己的规则、或者有人在门户里批准了它。
一条 `exec` 规则也从不授权同一条命令的提权形式。事件日志会记下每一条是走哪条通道跑的，
所以 `wanctl logs` 能回答*这台手机上以 root 跑过什么*。

开关关着的时候，那几条通道连探都不会探——一台已 root 的设备，
不会为一个没人打开的功能弹出 root 管理器的授权框。

### 动词

提权通道才是整个功能本身；这些动词只是给 adb 表面上那些在管道里不好使的部分做的简写。
每一个都是一条 shell 命令，走同一条通道，受同一个提权策略类别管，记进同一份审计日志——
这里没有任何一样东西是你没法自己敲出来的。

```sh
wanctl screenshot phone -o shot.png       # implies --elevate; "-o -" writes stdout
wanctl exec --target phone --elevate -- input tap 540 1200
wanctl exec --target phone --elevate -- input text 'hello world'
wanctl exec --target phone --elevate -- app list -3
wanctl exec --target phone --elevate -- app uninstall com.example
wanctl exec --target phone --elevate -- settings get global adb_wifi_enabled
wanctl exec --target phone --elevate -- prop get ro.product.model
wanctl exec --target phone --elevate -- logcat
```

| 动词 | 实际变成 | 为什么它是个动词 |
|---|---|---|
| `screenshot` | `screencap -p` | PNG 走 stdout；文件由控制端写 |
| `input tap/swipe/text/key` | `input …` | 参数经过校验，而且 `text` 始终是一个词 |
| `app list/info/install/uninstall/start/stop/clear` | `pm`、`am`、`monkey`、`dumpsys` | 一个名词盖住四个工具 |
| `settings get/put` | `settings …` | 那个 namespace 是一个只有三项的闭集 |
| `prop get/set` | `getprop`/`setprop` | — |
| `logcat` | `logcat -d -v time -t 200` | 默认是 dump：follow 在 exec 上永远不会返回 |

每一个参数在变成以 root 运行的 shell 源码之前都被引号包过，
所以一个含着 `; rm -rf /` 的包名，到达时就是一个包名。一条含有未加引号的展开
（`$(…)`、`$VAR`、一个管道）的命令，根本不会被当成动词，
而是作为一条普通的提权命令原样送过去——去猜它，比让 shell 干 shell 该干的事更糟。

`wanctl screenshot` 会先把图缓冲下来、核对 PNG 魔数，然后才写文件，
所以一次失败的截图不会留下一个看着像真的、其实被截断的文件。

**su 不是一个程序。** Magisk、KernelSU 和 APatch 吃 `su -c CMD`；
AOSP 自己的 su，也就是 userdebug 构建和模拟器镜像上那个，直接拒绝这种写法，
它要的是 `su root sh -c CMD`。探测会两种都试，留下以 root 应答的那一种，
于是两个家族谁都不用靠名字去识别。2026-08-14 在一个 android-29 的 `google_apis`
模拟器镜像上实测：

```
su -c id            → su: invalid uid/gid '-c'
su root sh -c id    → uid=0(root) … context=u:r:su:s0
```

同一天在那个镜像上验证过：开关关着时，通道会被列出来但以那个开关为理由拒绝；
开关开着时，`su` 探测到 `uid=0`，而
`settings get global adb_wifi_enabled`——在 PGBM10 上对一个 app uid 会抛异常的那条命令
——正常返回。

### 验过了，在什么上验的

在一台**小米 10 至尊版**（M2007J1SC，Android 11，Magisk）和一个 **android-29
模拟器**上，两者都从一台 Mac 经一条真实 relay 驱动，2026-08-14：

| | 小米 10 至尊版（Magisk） | 模拟器（AOSP su） |
|---|---|---|
| `exec -- id` | `uid=2000(shell)` | `uid=2000(shell)` |
| `exec --elevate -- id`（自动） | `uid=0(root)` `u:r:magisk:s0` | `uid=0(root)` `u:r:su:s0` |
| `exec --elevate --via adb -- id` | `uid=2000(shell)` `u:r:shell:s0` | — |
| 经 adb 通道传回的退出码 | 42 → 42 | — |
| `screenshot` | 1080×2340 的 PNG | 1080×2280 的 PNG |
| `settings` / `prop` / `app list` | 都返回真实数据 | 都返回真实数据 |
| 事件日志 | `"via":"su"`、`"via":"adb"` | `"via":"su"` |

两种 su 调用形式都是真跑过的：Magisk 的 `su` 吃 `-c`，
模拟器的 AOSP `su` 拒绝了它、要的是 `su root sh -c`。一个只猜一种形式的构建，
会在这两台设备中的某一台上报「没 root」。

被测的那个 agent 是从一个 adb shell 里跑起来的，所以它*未提权*时的 uid 在那里是 2000，
而不是一个装好的 APK 会有的那个 app uid。这不影响那几条提权通道所证明的东西——
`su` 到达 uid 0、adb 通道到达 shell 域，怎么跑都是同样的操作——
但从 app 沙箱这个起点出发的那一段，仍然需要一次签名 APK 的运行来端到端确认。

### 重启之后怎么回来

两套机制，因为只靠一套不够可靠。

`BOOT_COMPLETED` 是快的那条路：开机完成之后大约一秒 agent 就回来了。
但这个广播是尽力而为的。在 PA2353 上对同一个构建做了四次重启，其中一次被直接丢掉——

```
am_broadcast_discard_app: [0,…,BOOT_COMPLETED,187,ResolveInfo{dev.wanctl.agent/.BootReceiver}]
```

——而同一个广播在同一秒里送到了别的 app。Android 会丢掉那些它起不来其进程的接收方，
而一台刚开机的平板当时的负载是 30。

所以 app 还会调度一个**持久化的周期任务**（15 分钟，JobScheduler 允许的最短值），
它在 agent「本该在跑却没在跑」时把它拉起来。广播被丢掉的最坏情况，
是这台设备晚了一刻钟，而不是一直缺席到有人发现。打开 app 会立刻对齐。

### 为什么那个二进制叫 `libwanctl.so`

因为这是把一个可执行文件放到 Android 设备上、并且让 app 被允许运行它的唯一办法。

Android 拒绝 `untrusted_app` 域去 `exec` 一个标着 `app_data_file` 的文件，
而每一个 app 写得进去的目录都带着那个标签。APK 的 `lib/<abi>/` 目录标的是
`apk_data_file`，`untrusted_app` *可以*执行它——但只有当 manifest 里写着
`android:extractNativeLibs="true"` 时，包管理器才会把它解压到磁盘上，
而且它只解压名字形如 `lib*.so` 的文件。所以 wanctl 是作为
`lib/arm64-v8a/libwanctl.so` 发出去的，在装好的设备上它是：

```
/data/app/~~…/dev.wanctl.agent-…/lib/arm64/libwanctl.so
  -rwxr-xr-x system system u:object_r:apk_data_file:s0
```

没有任何东西 `dlopen` 它。它是一个顶着库名字的程序。Termux 自己那个 24 MB 的
bootstrap 也是这么发的。

配置跟二进制不一样，它住在 app 的私有目录里（`files/wanctl`），
那里需要可写，不需要可执行。

### 更新

`wanctl update` 在这里不好用，而且它会明说。app 能执行的那个目录属于包管理器、是只读的，
所以更新的单位是 APK。

点**检查更新**。二进制会取来签名过的发布清单，用编译进它自己的那把密钥验证 Ed25519
签名和 APK 的 SHA-256，然后把验过的文件交给系统包安装器，
后者再拿 APK 签名跟已安装 app 的签名核对。要换掉任何东西，
两个互相独立的签名必须都点头。

第一次的时候，Android 会问你要不要允许 wanctl 安装应用。

## Termux

仍然支持，仍然有文档，但不再推荐。下面的内容一字未改，验证时间是 2026-08-06。

```sh
pkg install openssl-tool          # the installer verifies a signature with it
curl -fsSL https://relay.example.com/install.sh | sh
wanctl                            # Feishu login, then run detached
```

（安装器在 `/install.sh`，不在 `/dl/install.sh`。`/dl/` 只提供签名清单点名的东西，
所以这个文件一直写到 2026-08-07 的那个 URL——`/dl/install.sh`——返回的是 404，
文档里那条 Termux 一行命令从来就没成功过。对着生产 relay 确认过。）

Termux 要付出四个绕行手法，全在二进制内部，全都是同一条规则的后果：
*agent 在 Android 上执行的任何东西，都必须用它的系统绝对路径点名，
并且必须住在 app 私有数据目录之外*。Termux 自己也只有在预加载
`libtermux-exec.so` 之后才能运行它自己的二进制，那个库把每一次 `execve`
都改道去走动态链接器——一个不带 CGO 的 Go 二进制永远不会加载它，
于是它只吃到了这次拦截的副作用，却没吃到这次拦截的好处：

- **argv[0] 被复制了一份。** 链接器会在前面塞上程序解析出来的路径，
  于是 `wanctl version` 到达时变成 `[wanctl, /abs/path/wanctl, version]`，
  每个参数都晚了一格。构建里会检测并丢掉它。**v0.1.7 把这件事做错了**，
  从 `PATH` 上敲裸 `wanctl` 完全用不了；v0.1.8 修好了。
- **`os.Executable()` 返回的是链接器**，于是 `wanctl update`
  把升级目标解析成了 `/apex/com.android.runtime/bin/linker64`。
  在一台已 root 的设备上，那会覆盖掉系统链接器，把整个运行时搞挂。
- **`wanctl start` 没法执行它自己的二进制**，只能像 Termux 那样显式地去调链接器。
- **从 `PATH` 上找 `getprop` 会找到 Termux 那份副本**，那份同样不可执行，
  于是设备名静默退回成了 `wanctl-agent`。改用绝对路径 `/system/bin/getprop`。

这四条在 APK 里一条都不存在，因为没有任何东西拦截它的 exec。

`wanctl service install` 在 Android 上会拒绝：这个平台不给一个非特权进程任何可以装进去的
服务管理器。在 Termux 下最接近的替代是 `termux-wake-lock`、
单独的 **Termux:Boot** app，以及 `pkg install termux-services`。
**这三样 wanctl 一样都没验证过。**

## 什么都不装（adb）

对一台已经插着 USB 的设备有用。

```sh
adb push wanctl-android-arm64 /data/local/tmp/wanctl
adb shell chmod 755 /data/local/tmp/wanctl
adb shell /data/local/tmp/wanctl        # login + detached agent
```

`/data/local/tmp` 是 adb shell 用户唯一既写得进去又执行得了的地方。
注意它是跟其它每一个 app、以及任何一个握着 adb 的人共用的——APK 的私有目录不是。

## Android 上有什么不一样，为什么

**DNS。** Android 没有 `/etc/resolv.conf`——解析走 `netd`，
而只有 bionic 的 libc 够得着它。wanctl 发的是不带 CGO 的静态二进制，
所以 Go 的解析器会退回到 `127.0.0.1:53`，每一次查询都会以 `connection refused`
失败，一个包都到不了 relay。Android 构建改成把解析器指向明确的域名服务器
（阿里 DNS、Google、Cloudflare，轮换着来，好让一次重试落到另一家运营商上）。
完整推理在 `docs/adr/0002-android-resolver.md`。

relay 位于一个私有区或者分裂视图的区里时，覆盖掉它：

```sh
export WANCTL_DNS=10.0.0.53,10.0.0.54     # comma-separated, :53 assumed
```

一台确实有 `/etc/resolv.conf` 的设备（proot 发行版、已 root 的环境）不受影响。
Termux 自己的 `$PREFIX/etc/resolv.conf` 存在时会被读取。

**配置目录。** app 会显式传 `WANCTL_CONFIG_DIR`。其它情况下：
在 Termux 里 `$HOME` 是真的，配置落在 `$HOME/.config/wanctl`；
在 adb shell 下 `HOME=/` 是只读的，于是 agent 退到 `$TMPDIR/wanctl`，
再退到二进制旁边的 `.wanctl`，再退到 `/data/local/tmp/.wanctl`。

**Shell。** 会话跑在 `/system/bin/sh`（mksh）里，那是每一个 Android 构建上都有的那个 shell。
`/bin/sh` 从 Android 11 起才存在。Termux 自己的 `$PREFIX/bin/sh`
即便在 Termux 里也是刻意不用的——agent 物理上就起不动它。你什么也没损失：
一个 `/system/bin/sh` 会话继承 Termux 的 `PATH`，而*由那个 shell 启动的* Termux
二进制照常运行，因为那几次 exec 是 mksh 做的。经 relay 验证过：`git --version` → 2.52.0，
`python --version` → 3.12.12，两个都是 Termux 的。

**文件传输。** Android 给 shell 用户的，是通往任何有用位置的那些目录的「只可穿越」权限
（`/data` 是 `drwxrwx--x system:system`），而 `os.Root` 必须一层一层地*打开*下去。
于是把一次传输绑到卷根上，哪怕目标目录可写，也会以
`openat data/local/tmp: permission denied` 失败。在 Android 上，
一次原本不受约束的传输改为绑到目标自己所在的那个目录——比卷根窄，不比它宽，
而且目标处的软链接照样被拒绝。

**设备名。** 每一台 Android 设备报出来的主机名都是 `localhost`，
所以 agent 转而通过 `/system/bin/getprop` 去问属性服务
（先 `ro.product.marketname`，再 `ro.product.model`），注册成比如 `pa2353`。

## 构建 APK

```sh
./scripts/build-apk.sh              # dev build, debug-signed
./scripts/build-apk.sh v0.1.11      # release build; needs the keystore env
```

它需要 Android SDK（`ANDROID_HOME`，或者 `~/Library/Android/sdk`）和一个 JDK
（Android Studio 自带的那个会被自动找到）。没有 Gradle，也没有 AndroidX；
整条链是 `aapt2 → javac → d8 → zipalign → apksigner`，所以构建不需要网络。
代价——你没法在 Android Studio 里打开 `android/`——在
`docs/adr/0003-android-apk.md` 里论证过。

发布签名读的是 `WANCTL_ANDROID_KEYSTORE`（或 `..._B64`）、
`WANCTL_ANDROID_KEYSTORE_PASS` 和 `WANCTL_ANDROID_KEY_ALIAS`。这些都没有的话，
它会退回到 debug 密钥并说明这一点——这样的 APK 能装在开发者自己的设备上，
对别人一文不值。**丢了发布 keystore 意味着每一台已安装的设备都必须卸载重装**；
Android 对签名密钥变更没有任何恢复路径。

`scripts/build-release.sh` 会捡起暂存在
`build/android/wanctl-android-arm64.apk` 的 APK，或者自己构建一个，
并且在没有 `WANCTL_SKIP_APK=1` 说可以的情况下拒绝切一个发布——
一份没有 APK 条目的清单，会把每一台已装 app 的设备撂在它当前的版本上。

## 设备明明「在运行」，控制端却看不见它

app 上显示 ● 运行中，意思是 agent 进程活着。而 *relay* 是不是还把这台设备列在名单上，
是另一件事，这两件事可以不一致：agent 启动时会打印一次「online via …」，
之后再也不会推翻它，而它的注册是靠一个可能静默失败的轮询循环维持的。

2026-08-07 起，那个轮询循环会说话了。去 查看日志（或者
`adb logcat -s wanctl:I`）里找：

```
wanctl: relay poll failed: … write: software caused connection abort (1 consecutive)
```

Wi-Fi 切换前后出现零星几次是正常的，也会自愈——上面那个例子在下一次尝试时就恢复了。
计数一直往上爬，才说明这台设备是真的到不了 relay，
接下来该查的是网络有没有把 Android 构建所用的那些公共 DNS 解析器挡掉
（见 ADR 0002 和 `WANCTL_DNS`），因为在 wanctl 不通的时候，
`ping` 和浏览器会照常好用。

## 在设备上测试

- **`adb install --no-incremental`。** 默认的增量安装会把 APK 留在 `incremental-fs`
  上；adb 一断开，那里面的每一次读取都以 `ETIME` 失败（`Timer expired`，exec 退出码
  126）——它看起来跟一次 SELinux 执行拒绝一模一样，但它不是。
- **`run-as` 不是 app 所在的域。** 它跑在 `runas_app` 里，不是 `untrusted_app`。
  当探针有用，当证据一文不值。
- **一个发布签名的 APK 是不可调试的**，所以 `run-as` 和 `adb shell cat files/…`
  对它都没用。去读 `adb logcat -s wanctl:I`，或者 app 自己的 查看日志 页面。

## 验过了，在什么上验的

2026-08-07 在一台 vivo PA2353（Android 13，arm64）上，对着**生产** relay，
从一台 Mac 驱动——装好的 APK，走 app 界面，wanctl 这一侧不走任何 adb 捷径：

- 二进制**在 `untrusted_app` 域里**从 `nativeLibraryDir` 执行
  （`id -Z` → `u:r:untrusted_app:s0:c10,c257,c512,c768`），
  这一点 `run-as` 证明不了，因为它跑在 `runas_app` 里
- 通过 app 自己的 登录 按钮完成接入（`login --code`），令牌已存，
  设备注册成了 `pa2353`
- `exec` 带流式输出和真实退出码（发 42，回 42）
- 512 KB 的 `push` 和 `pull`，逐字节一致的往返
- **重启 ×2，屏幕睡着，没人碰这台设备 → 从 Mac 上照样可控**——
  这正是 Termux 那条路从来没做到过的事
- `update --fetch-apk` 从一台 relay 下载并验证了一个签名过的 APK，
  哈希与构建出来的产物一致，而已是最新版那条分支返回空 stdout 和退出码 0

以及在 2026-08-07，v0.1.11 部署好、`/dl` 第一次提供 APK 之后，
**整条应用内更新链路**：检查更新 → PackageInstaller → 跑起新构建，
全程在 app 自己的界面里。事后是从控制端核的，不是听 app 自己说的：

- 装好的 `base.apk` 与发布出去的产物逐字节同哈希
  （`sha256sum $(pm path dev.wanctl.agent)` 对着 `/dl` 清单里的那一条）
- app 执行的那个二进制报的是 `v0.1.11`，`id -Z` 仍然是
  `u:r:untrusted_app:s0`
- **设备的身份指纹没有变。** 这才是值得留下的那一点：密钥住在 app 的私有数据里，
  只有原地升级才能让它活下来。签名不匹配会逼出一次卸载，
  从而产生一个新指纹，所以这一点正好证明了那两条签名链——
  清单的 Ed25519 和 APK 的证书——各自独立地都点了头。

## 还没验过的

- **任何一台不是 vivo PA2353 的设备。** OEM 那几道闸（自启动名单、后台耗电管理器）
  各家不同，也是「它重启之后就再也不回来了」在一台没人试过的设备上最可能的原因。
- **Termux:Boot / termux-services**——它们是 Termux 那边有文档的、
  用来扛过重启的机制，不是一个测试过的 wanctl 集成。
