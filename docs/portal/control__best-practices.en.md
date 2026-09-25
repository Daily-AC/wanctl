# Getting the best out of wanctl

Every wanctl command goes through the relay, and it makes several round trips
there: it opens a connection, runs an end-to-end TLS handshake with the device,
and only then runs the command. Any delay added on the way from your machine to
the relay is paid several times per command, so that stretch of network is what
decides how smooth wanctl feels.

## Behind a proxy, send the relay direct

With a proxy client in TUN mode, traffic to the relay is routed by the proxy's
rules, and it easily lands on a node far away. One measurement (2026-09-25,
Beijing): a Mac running Clash Verge in TUN mode had its traffic to a Hong Kong
server routed through a Los Angeles node, with a median time to first byte of
1.25 s and a worst case of 3.9 s; at the same moment a direct connection from
home took 0.11 s.

First find out which relay you use. It is the `relay` line of the output (it
is also in `wanctl status`):

```
wanctl config
```

Then add a DIRECT rule for that domain, at the very top of your rules so that
no earlier rule from your subscription catches it first. In a mihomo / Clash
config file it looks like this:

```yaml
rules:
  - DOMAIN,relay.example.com,DIRECT
  # ...your existing rules
```

In Clash Verge Rev, do not edit the subscription file itself: the next
subscription update overwrites it whole. Open the subscription's menu, choose
"Edit Rules", and add the line with "Prepend Rule". Afterwards find the relay's
domain on the Connections page and check that it goes DIRECT. Other proxy
clients work the same way.

## wanctl only uses proxies from environment variables

wanctl does not read system proxy settings: the proxy in macOS network
settings, Windows Internet Options, and a proxy client's "system proxy" switch
have no effect on it. It reads only the `HTTPS_PROXY`, `HTTP_PROXY` and
`NO_PROXY` environment variables (either case). That leaves three situations:

- The proxy client only has "system proxy" on, TUN is off, and no proxy
  variable is set in your terminal: wanctl already connects directly, nothing
  to do.
- TUN is on: add the DIRECT rule from the previous section.
- `HTTPS_PROXY` is set in your terminal: wanctl goes through that proxy, and it
  does not use HTTP/3.

In the third case, add the relay to `NO_PROXY` so wanctl connects directly.
Where UDP gets through, wanctl switches to HTTP/3 by itself, which is much
faster on networks that throttle TCP; through a proxy it cannot:

```
export NO_PROXY="$NO_PROXY,relay.example.com"
```

Note that an agent installed as a service with `wanctl service install` does
not see variables you set in your terminal or shell profile, while an agent
launched from a terminal with `wanctl start` inherits that terminal's
variables.

The other way round: if your network lets UDP through but carries it worse than
TCP (some TUN-mode proxies do), set `WANCTL_HTTP3=0` to keep wanctl on HTTP/2.
See the [environment reference](https://wc.z10.dev/docs/environment/).

## Large files: push up to 1 GiB, use another tool beyond that

`wanctl push` takes at most 1 GiB per file. A larger file is refused before
anything is sent:

```
wanctl: upload size 1073741825 outside supported range 0..1073741824
```

`pull` has no such limit, but both go all the way through the relay, so their
speed depends on each end's network to the relay, and an interrupted transfer
starts over from the beginning. So:

- A directory or a pile of small files: pack it into one archive first. `push`
  and `pull` move one file at a time, and every file opens a new connection to
  the device.
- Anything over 1 GiB, or anything large you move repeatedly: if the two
  machines can reach each other over SSH, use `scp` or `rsync` (`rsync` can
  resume); if they cannot, go through cloud storage or an object store.

## Install controlled machines as a service, not just wanctl start

`wanctl start` puts the agent in the background. It survives closing the
terminal, but not logging out or rebooting; `wanctl login` only saves a
credential and starts no agent at all. A machine that has only ever run those
two goes offline at the first reboot. For a machine you want to control long
term, sign in once and then install the service (stop the agent `wanctl start`
launched first, so the two do not fight over one config directory):

```
wanctl stop
wanctl service install
wanctl service status
```

`service install` sets up a launchd agent on macOS, a systemd user service on
Linux and a scheduled task on Windows, and restarts the agent if it exits
unexpectedly. On macOS and Windows it starts once a user logs in, so an
unattended machine needs automatic login. On Linux it also tries
`loginctl enable-linger`; if that works the agent comes up at boot without a
login, and if not it tells you to run it again with sudo. To retire the service
later, run `wanctl service uninstall`.

## When it feels slow, check yourself

Go through these in order; each step rules out one cause.

**Versions first.** Run `wanctl version` here and
`wanctl status --target DEVICE` for the other end, which reports that device's
agent version. HTTP/3 arrived in v0.13.0. Agents update themselves by default;
on the controller, run `wanctl update`.

**Then check for a detour.** `env | grep -i proxy` shows whether your terminal
sets a proxy variable. With TUN on, find the relay's domain in your proxy
client's connection list and check that it is DIRECT. If it is not, go back to
the first two sections.

**Then measure the path itself.** Once the first two steps are sorted, run this
a few times. It prints the time to first byte from the relay, in seconds:

```
curl --noproxy '*' -so /dev/null -w '%{time_starttransfer}\n' https://relay.example.com/healthz
```

For reference, the direct connection in the measurement above took 0.11 s. If
you are already direct and this number is still large, the path from your
network to the relay is slow in itself. There is nothing to tune on the wanctl
side; move large files with another tool as described above.

**Last, see whether HTTP/3 is helping.** wanctl does not show which protocol it
is using, but you can compare on the controller: push the same file of a few
tens of MB several times normally and several times with HTTP/3 off. Network
speed wobbles, so a single run of each proves nothing.

```
time wanctl push --target DEVICE ./test.bin /tmp/test.bin
time WANCTL_HTTP3=0 wanctl push --target DEVICE ./test.bin /tmp/test.bin
```

If it is clearly faster with HTTP/3 off, your network lets UDP through but
carries it badly, so keep `WANCTL_HTTP3=0` from now on. If the two are about
the same, HTTP/3 is not in use (for example because UDP is blocked and wanctl
has already fallen back to HTTP/2), or the bottleneck is elsewhere.
