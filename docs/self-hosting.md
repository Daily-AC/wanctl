# Self-hosting wanctl

This guide runs Postgres, the relay, and the portal on one host. The host needs
two DNS names, inbound HTTPS, Docker Engine, and Docker Compose. The examples
use `relay.example.com` and `portal.example.com`; replace both everywhere.

## 1. Create a GitHub OAuth App

1. Open GitHub **Settings -> Developer settings -> OAuth Apps** and choose
   **New OAuth App**.
2. Set **Homepage URL** to `https://portal.example.com`.
3. Set **Authorization callback URL** to
   `https://portal.example.com/auth/callback`. The scheme, host, and port must
   exactly match the portal's public origin.
4. Create a client secret and keep the client ID and secret for the next step.

## 2. Configure and start the services

From the repository root:

```bash
cp selfhost/.env.example selfhost/.env
```

Edit `selfhost/.env`, replace the two public origins and GitHub credentials,
and generate every local secret as described in its comment. Then start the
stack:

```bash
docker compose --env-file selfhost/.env -f selfhost/docker-compose.yml up -d
```

The relay waits for Postgres, runs its embedded migrations, and then reports
healthy. The portal starts only after that. Inspect startup with:

```bash
docker compose --env-file selfhost/.env -f selfhost/docker-compose.yml ps
docker compose --env-file selfhost/.env -f selfhost/docker-compose.yml logs relay portal
```

Postgres and the portal identity live in named volumes. `docker compose down`
keeps them; `docker compose down -v` permanently removes them.

## 3. Put HTTPS in front

Both application ports bind only to loopback. A minimal Caddyfile is:

```caddyfile
relay.example.com { reverse_proxy 127.0.0.1:8080 }
portal.example.com { reverse_proxy 127.0.0.1:8081 }
```

Equivalent nginx server blocks are:

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

wanctl uses finite HTTP long-poll requests by default, so WebSocket upgrade
support and streaming-specific timeouts are not required. Disabling nginx
response buffering is sufficient; no other special proxy behavior is needed.

## 4. Sign in and enroll devices

Open `https://portal.example.com`. On a new database, the first GitHub account
to complete login becomes the administrator. Later accounts remain on the
pending page until invited.

Create an invite code for a second user from the repository checkout:

```bash
WANCTL_RELAY=https://relay.example.com \
WANCTL_ADMIN_SECRET='<value from selfhost/.env>' \
go run . admin invite
```

Alternatively, pre-approve a specific GitHub login with `admin invite --github
LOGIN`. Give the one-time code to the user; the pending page accepts it.

After installing or building the `wanctl` binary on a device, enroll and start
it with one command:

```bash
WANCTL_PORTAL=https://portal.example.com WANCTL_RELAY=https://relay.example.com wanctl
```

The command opens the portal, asks for the one-time enrollment code shown
there, stores the issued token, and starts the agent. The agent makes outbound
connections only.

### Optional: enable the portal device console

The portal can issue tokens and enroll devices without a portal token. Its live
device console additionally needs a privileged token in the reserved `portal`
namespace. After the relay is running, export the admin secret and issue one:

```bash
export WANCTL_ADMIN_SECRET='<value from selfhost/.env>'
curl -fsS https://relay.example.com/admin/tokens/issue \
  -H "X-Admin-Secret: $WANCTL_ADMIN_SECRET" \
  -H 'Content-Type: application/json' \
  --data '{"namespace":"portal","label":"self-hosted portal","days":0}'
```

Set the returned `wanctl_...` value as `WANCTL_PORTAL_TOKEN` in
`selfhost/.env`, then apply it with:

```bash
docker compose --env-file selfhost/.env -f selfhost/docker-compose.yml up -d portal
```

## Troubleshooting

**GitHub reports a callback URL error.** The OAuth App callback must exactly
match `PORTAL_PUBLIC_ORIGIN` plus `/auth/callback`. Check the scheme, hostname,
port, and any stale example value in `selfhost/.env`.

**The portal exits with a session-secret error.** `WANCTL_SESSION_SECRET` is
measured in bytes and must be at least 32 bytes. `openssl rand -hex 32` produces
a suitable 64-character value. Recreate the portal after changing it.

**The relay cannot connect to `DATABASE_URL`.** It pings Postgres before
serving and exits if the connection or embedded migration fails. Compose then
restarts it under `restart: unless-stopped`; inspect `docker compose logs relay
postgres`, and verify the database credentials. Do not disable migrations on a
fresh database.
