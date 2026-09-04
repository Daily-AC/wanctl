# 发布签名

wanctl 的每个发布都带着**对同一份清单字节的两个签名**。relay 是一个不可信的镜像：
它可以提供发布文件，但无论是更新器还是安装器，都不会接受一个二进制，
除非它的精确元数据出现在一份有效的签名清单里。

| 文件 | 算法 | 由谁验证 | 密钥变量 |
|---|---|---|---|
| `manifest.json.sig` | Ed25519 | `wanctl update`、relay `/dl/*` 的准入 | `WANCTL_RELEASE_SIGNING_KEY` |
| `manifest.json.rsa.sig` | RSA-3072 PKCS#1 v1.5，SHA-256 | `install.sh`、`install.ps1` | `WANCTL_RELEASE_RSA_KEY` |

用两种算法，是因为这两个验证方的约束不一样。`wanctl update` 在 Go 里面验，
那里 Ed25519 最理想，而且不带任何依赖。安装脚本是在一个未经改造的 shell 里验，
而在我们要发的三个平台中，有两个够不到 Ed25519：

- **macOS** 的 `/usr/bin/openssl` 是 LibreSSL，它的 `pkeyutl` 没有 `-rawin`。
  不单独装一个 OpenSSL 就根本验不了 Ed25519，于是 `curl … | sh`
  在任何一台原生 Mac 上都失败。
- **Windows PowerShell** 压根没有 Ed25519。要拿到它就得先装 OpenSSL，
  那是一次好几分钟的下载，源站慢到连 `winget` 都会超时。

RSA 在两边都能原生验证——`openssl dgst -verify` 在 LibreSSL 上可用，
而每个 Windows 都自带的 PowerShell 5.1 里有 `RSACryptoServiceProvider`。
在采用这个方案之前，两条路都在真机上端到端验过。

更新器保留 Ed25519 而不是把所有东西都换成 RSA，是为了让已经发出去的二进制照常升级：
一个 v0.1.2 的二进制用编译进它自己的那把 Ed25519 公钥验证，从不看 RSA 签名。

## 信任引导

最强的引导方式，是从那个可独立认证的 GitHub 发布页去取 `install.sh` 或
`install.ps1`，或者从一个审过的 Git commit 构建出来。把安装器和发布说明放在一起，
好让它内嵌的公钥可被审计。如果 relay 被攻破了，攻击者可以同时替换掉从它这里提供的脚本
和那份副本里内嵌的公钥——签名验证救不了一个攻击者同样控制着的脚本。

relay 还是照样提供安装器，因为一行安装才是大多数人真正会跑的那个，
而一条被跳过的加固路径谁也保护不了。把那条路当成「对着一台可能已被攻破的 relay
自己的密钥验过」，机器要紧的时候优先走 GitHub 发布。两条路的验证都仍然是失败即关闭：
一个能替换 `/dist` 里的二进制、却替换不了被提供出去的脚本的攻击者，什么都拿不到。

官方安装器烤进的是 GitHub 发布基址，relay 那一格留空。自部署的人可以改成带一个默认
relay 构建，去用一份签名过的 `/dl` 镜像。`WANCTL_DIST_BASE` 或 `WANCTL_RELAY`
在安装时可以覆盖其中任意一个来源。

发布构建会把一组逗号分隔的、可信的原始 Ed25519 公钥注入
`internal/release.TrustedPublicKeys`。普通构建没有信任密钥；
因此 `wanctl update` 和 relay 的 `/dl/*` 分发在那里都是关着的。

## CI 秘钥

在一台离线的管理机上生成两把密钥：

```sh
# WANCTL_RELEASE_SIGNING_KEY — Ed25519 seed, for wanctl update
openssl rand 32 | base64

# WANCTL_RELEASE_RSA_KEY — for the install scripts. Either DER encoding is
# accepted (genpkey emits PKCS#1 for RSA; other tooling emits PKCS#8).
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -outform DER | base64 | tr -d '\n'
```

把两者都存成 `release` 这个 GitHub environment 上的 secret，
并把它的部署 tag 策略限制在发布 tag 上。Base64 让它们保持单行，
而单行让秘钥打码可靠；RSA 那个值约 2.4 KB，离 GitHub 的 secret 大小上限还很宽裕。
丢了 RSA 私钥对已有的安装不致命——它只给新装的签——但确实会逼着下一个发布轮换公钥。
不要把它存进仓库、Docker 构建参数、镜像层、relay 文件系统，或者某个通用应用的环境里。
仓库还必须强制保护 `v*` tag、设一个必需的 environment 审阅者并关掉管理员绕过、
以及不可变发布。这些是仓库设置，不是 workflow 文件的属性；
2026-08-28 那次审计发现它们都不在，所以发布仍然卡在这项运维动作上。
只要缺任何一个签名密钥或 Android keystore 输入，发布 job 就会中止。

这个 workflow 会用配套的公钥构建每个平台的二进制，生成 `manifest.json`，
对它的精确字节签出 `manifest.json.sig`，然后把整个发布目录作为附件上传到 GitHub
Release 上。仓库的不可变发布保护必须单独打开。这个目录里还有
`release-public.pem`，它让维护者能在没有私有签名密钥的情况下独立验证下载下来的发布产物。
把那个目录以只读方式部署到 `WANCTL_DIST_DIR`。relay 在提供任何东西之前，
会验证清单和每一个文件。目录不对或者不全，所有 `/dl/*` 路径都返回 HTTP 503。
用公开的发布元数据构建 relay 镜像（公钥不是秘密），然后把同一份签名过的目录只读挂上去。
标准的自部署路径把这些值经 `selfhost/.env` 和 Compose 传进去：

```sh
cd selfhost
cp .env.example .env
# Set WANCTL_VERSION and WANCTL_RELEASE_PUBLIC_KEYS in .env, then:
docker compose build
```

本地做一次发布彩排：

```sh
export WANCTL_RELEASE_SIGNING_KEY='<base64 32-byte seed>'
./scripts/build-release.sh v1.2.3
go run ./cmd/release-manifest verify release release/release-public.pem
```

推一个受保护的 `vMAJOR.MINOR.PATCH` tag 会触发
`.github/workflows/release.yml`。这个 workflow 需要两个签名秘钥，
外加 Android keystore 和密码。它会发布全部四个按 ABI 分的 APK；
一个没有 APK 的发布会被拒绝，因为那会把已安装的 Android 设备撂在半路上。

要手工发布同一个版本，构建或下载完整的 `release/` 目录，检出那个精确的 tag，
给 `gh` 做好认证，然后跑：

```sh
./scripts/publish-release.sh v1.2.3 release internal/portal/changelog/v1.2.3.md
```

发布器会拒绝：已存在的发布、tag 与源码不匹配、多出或缺失的文件、清单与 tag 不匹配、
坏签名或坏产物哈希，以及内嵌公钥与 `release-public-rsa.pem` 不一致的安装器——
Unix 安装器带的是 PEM，Windows 那个带的是 .NET XML。

发布不需要任何一把私钥：两个签名都是对着发布目录里带的公钥验的。
把 `release-public.pem` 对着上一个发布（或者一份保存在产物之外的副本）钉住——
一个既提供密钥又提供签名的产物，只能证明它自己内部自洽。

## 轮换与吊销

1. 生成一把新的离线密钥，把它作为新的 CI 签名秘钥保护起来。
2. 在 CI 还用旧密钥签的时候，把 `WANCTL_RELEASE_PREVIOUS_PUBLIC_KEYS`
   设成新公钥，发一个过渡版本。别被变量名骗了，这个值是一把额外的信任密钥；
   发布前检查一下产出的二进制和清单。
3. 等到在维护范围内的机群都装上了那个过渡版本。
4. 把 `WANCTL_RELEASE_SIGNING_KEY` 切成新私钥。把
   `WANCTL_RELEASE_PREVIOUS_PUBLIC_KEYS` 设成旧公钥再发一个重叠期版本，
   之后的版本里去掉它。
5. 重新部署签名过的发布目录。绝不要给一个已有版本重新签名；
   发一个严格更大的语义版本号。

如果旧私钥泄漏了，就跳过重叠期：把它的公钥删掉，发一个更高版本、
由一把已经被信任的恢复密钥签的发布。那些从没收到过恢复密钥的设备，
需要通过独立的 GitHub/源码信任路径去拿一个安装器。
把吊销掉的密钥指纹和受影响的版本区间记进发布说明。

## 失败时的行为

更新器会拒绝：未签名或格式错误的清单、未知字段、未知签名密钥、坏签名、缺平台、
同版本重放、降级、超过 64 MiB 的产物、大小不符，以及 SHA-256 不符。
校验在临时文件里全部完成，之后才 chmod 或替换二进制。
`published_at` 是签名过的审计元数据，不是一道挂钟闸门：设备的时钟不被信任。
单调递增的语义版本号提供的是重放和降级防御。
