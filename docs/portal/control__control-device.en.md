# Control another device from your computer

Once the tool is installed on the controller (your own computer), set the
instance addresses first — once is enough, and `wanctl config` shows and
changes them whenever you like:

```
wanctl config set relay=https://relay.example.com portal=https://portal.example.com
```

`wanctl login` signs you in once and gets you an identity (portal login in the
browser, then paste the one-time code back into the terminal). Then just use it:

```
wanctl peers                                  # which devices are online
wanctl exec --target DEVICE "uname -a"        # run a command on it
wanctl push --target DEVICE ./local /remote   # send a file up
wanctl pull --target DEVICE /remote ./local   # pull a file down
```

The device owner can give a device an alias: open the device in the portal,
click the gear on the title row, fill in the **Alias** field and save; clearing
it and saving again removes it. An alias is unique within your namespace and
may not collide with any device's real name.

Once set, the device list and the device page show the alias as the display
name, with the real device name beside it — an alias is an easier handle, not a
way to hide which machine it is. `wanctl peers` prints the alias in brackets
after the real device name, and the `--target` of the commands above takes
either the real device name or the alias. For a shared device, write
`owner-namespace/alias` the same way.

The first time you drive a new device, it waits for that device's own
**Waiting** / **Trusted controllers** page to say yes. Driving a friend's device
needs a share first (see "Invites, friends and sharing").
