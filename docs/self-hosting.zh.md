# 自己部署一套 wanctl

这份指南在同一台主机上跑起 Postgres、relay 和门户。这台主机需要两个 DNS 名字、
入站 HTTPS、Docker Engine 和 Docker Compose，还要大约 1.5 GB 磁盘放镜像：
第一次启动会在 Docker 里从源码构建 wanctl，要拉 Go 和 Postgres 的基础镜像，
编译几分钟。例子里用的是 `relay.example.com` 和 `portal.example.com`，
两个都请全文替换掉。

## 1. 创建一个 GitHub OAuth App

1. 打开 GitHub **Settings -> Developer settings -> OAuth Apps**，选
   **New OAuth App**。
2. **Homepage URL** 填 `https://portal.example.com`。
3. **Authorization callback URL** 填
   `https://portal.example.com/auth/callback`。协议、主机名和端口必须跟门户对外的
   origin 完全一致。
4. 创建一个 client secret，把 client ID 和 secret 留着，下一步要用。

## 2. 配置并启动服务

这份指南里所有 compose 命令都在 `selfhost/` 目录下跑，Compose 会自己在那里认出
`.env` 和 `docker-compose.yml`：

```bash
cd selfhost
cp .env.example .env
chmod 600 .env
```

编辑 `.env`：把 `RELAY_PUBLIC_ORIGIN` 和 `PORTAL_PUBLIC_ORIGIN` 设成你那两个对外的
origin，把第 1 步拿到的 `WANCTL_GITHUB_CLIENT_ID` 和 `WANCTL_GITHUB_CLIENT_SECRET`
填进去，其余每个本地 secret 都按它注释里写的办法生成。这个文件装着这套部署的全部
秘钥，`chmod` 就是为它准备的。然后启动整个栈：

```bash
docker compose up -d
```

第一次启动会先在 Docker 里把 wanctl 编出来，什么都还没起。如果主机连不上
`proxy.golang.org`，构建会卡在模块下载超时上——在 `.env` 里把 `GOPROXY` 设成一个你
连得上的镜像（比如 `GOPROXY=https://goproxy.cn,direct`），再跑一次
`docker compose build`。

relay 会等 Postgres，跑完它内嵌的 migration，然后才报健康。门户在那之后才启动。
用这两条看启动过程：

```bash
docker compose ps
docker compose logs relay portal
```

全新部署上有两行日志看着像出错，其实是预期之内的：
`release distribution disabled: read manifest: ...`（没挂签名过的发布目录）和
`lark approval disabled: ...`（可选的 Lark 集成没配）。这两样都不影响本指南里的任何事。

Postgres 和门户身份放在具名卷里。`docker compose down` 会保留它们；
`docker compose down -v` 会永久删掉。

## 3. 前面架上 HTTPS

两个应用端口都只绑在 loopback 上。一份最小的 Caddyfile 是：

```caddyfile
relay.example.com { reverse_proxy 127.0.0.1:8080 }
portal.example.com { reverse_proxy 127.0.0.1:8081 }
```

等价的 nginx server 块是：

```nginx
server {
    listen 443 ssl;
    server_name relay.example.com;
    # Configure ssl_certificate and ssl_certificate_key for this name.
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
    }
}

server {
    listen 443 ssl;
    server_name portal.example.com;
    # Configure ssl_certificate and ssl_certificate_key for this name.
    location / {
        proxy_pass http://127.0.0.1:8081;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
    }
}
```

wanctl 默认用有限长度的 HTTP 长轮询请求，所以不需要 WebSocket 升级支持，
也不需要为流式做特别的超时设置。把 nginx 的响应缓冲关掉就够了，
没有别的代理行为需要特殊照顾。

往下走之前，把这条链路的每一段都确认一遍：

```bash
curl -s http://127.0.0.1:8080/healthz             # relay, direct: prints "ok"
curl -s https://relay.example.com/healthz          # relay, through the proxy: "ok"
curl -s https://portal.example.com/healthz         # portal, through the proxy: "ok"
curl -s -o /dev/null -w '%{http_code}\n' https://portal.example.com/   # 303 (redirect to login)
```

这两个 `/healthz` 端点也正是可用性监控该盯的目标；注意应用回应的是 GET，不是 HEAD。

## 4. 登录并接入设备

打开 `https://portal.example.com`。在一个全新的数据库上，第一个走完 GitHub 登录的账号
成为管理员。之后的账号会停在等待页上，直到被邀请。

用 relay 容器里已经带着的管理 CLI 给第二个用户开一个邀请码
（主机上除了 Docker 什么都不需要）：

```bash
docker compose exec -e WANCTL_RELAY=http://127.0.0.1:8080 relay wanctl admin invite
```

也可以加上 `--github LOGIN` 直接预先放行某个 GitHub 账号。把一次性的码给那个人，
等待页收这个码。

在设备上，从项目发布页装上签名过的二进制，把它指向你这套实例
（会持久化；之后用 `wanctl config` 查看和修改），然后接入并启动：

```bash
curl -fsSL https://github.com/Daily-AC/wanctl/releases/latest/download/install.sh | sh
wanctl config set relay=https://relay.example.com portal=https://portal.example.com
wanctl
```

最后那条命令会打开门户，问你门户上显示的那个一次性接入码，存下签发的令牌，
然后把 agent 启动起来。agent 只发起出站连接。（什么都没配的情况下直接跑 `wanctl`，
它会在终端里问你要这两个 URL。）

### 可选：打开门户的设备控制台

门户不需要门户令牌就能签发令牌、接入设备。它那个实时设备控制台则额外需要一个在保留
命名空间 `portal` 里的特权令牌。relay 起来之后，导出管理 secret，签发一个：

```bash
export WANCTL_ADMIN_SECRET='<value from selfhost/.env>'
curl -fsS https://relay.example.com/admin/tokens/issue \
  -H "X-Admin-Secret: $WANCTL_ADMIN_SECRET" \
  -H 'Content-Type: application/json' \
  --data '{"namespace":"portal","label":"self-hosted portal","days":0}'
```

把返回的 `wanctl_...` 值填进 `selfhost/.env` 的 `WANCTL_PORTAL_TOKEN`，然后让它生效：

```bash
docker compose up -d portal
```

### 可选：给设备提供签名过的发布包

打开发布分发之后，设备一行就能装上，之后用 `wanctl update` 升级，两者都对着项目的
发布签名验过。从 GitHub Releases 把某个版本的产物下载到 `selfhost/dist/`，
然后告诉 relay 该信任哪把签名密钥——这个信任锚是构建时编进去的，
刻意不做成运行时配置：

```bash
gh release download v0.2.0 --repo Daily-AC/wanctl --dir dist   # or curl each asset
# Derive the raw base64 key the relay build expects from the release's PEM:
openssl pkey -pubin -in dist/release-public.pem -outform DER | tail -c 32 | base64
```

把那个值填成 `.env` 里的 `WANCTL_RELEASE_PUBLIC_KEYS`，然后重新构建并重启 relay：

```bash
docker compose build relay && docker compose up -d relay
```

relay 的日志会从 `release distribution disabled` 变成开始提供 `/dl/*`，
一台连不上 GitHub 的设备就能从这个镜像装：

```bash
curl -fsSL https://relay.example.com/install.sh | sh
```
```powershell
irm https://relay.example.com/install.ps1 | iex
```

什么都不用导出：relay 会把它自己提供的安装脚本改写成指向它自己的 `/dl`——
因为能从这台 relay 取到脚本的人就能到达这台 relay，而这些人往往到不了脚本原本编译时
指向的那个发布页。`WANCTL_RELAY` 和 `WANCTL_DIST_BASE` 仍然能覆盖它，
从 GitHub 发布页取的那份副本则不受影响。这需要 `RELAY_PUBLIC_ORIGIN`
（compose 文件把它作为 `WANCTL_PUBLIC_ORIGIN` 传给 relay）。

这样装上的二进制，`wanctl update` 仍然是对着构建时烤进去的那个发布页跑的。
在一个到不了它的网络里，把它们一次性指向镜像：

```bash
wanctl config set release_base=https://relay.example.com/dl
```

这个镜像是可选的：官方二进制和安装器在安装和 `wanctl update` 上都默认走项目的 GitHub
发布页，所以大多数部署根本不需要提供 /dl。

## 排障

**GitHub 报 callback URL 错误。** OAuth App 的 callback 必须精确等于
`PORTAL_PUBLIC_ORIGIN` 加上 `/auth/callback`。检查协议、主机名、端口，
以及 `selfhost/.env` 里有没有残留的示例值。

**门户以 session-secret 错误退出。** `WANCTL_SESSION_SECRET` 是按字节算的，
至少要 32 字节。`openssl rand -hex 32` 生成的那个 64 字符的值正合适。
改完之后重建门户容器。

**relay 连不上 `DATABASE_URL`。** 它在开始服务之前会 ping Postgres，
连接或内嵌 migration 失败就退出。Compose 随后会按 `restart: unless-stopped`
把它重启起来；去看 `docker compose logs relay postgres`，核对数据库凭据。
不要在一个全新的数据库上关掉 migration。
