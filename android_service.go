package main

import (
	"fmt"
	"os"
	"strings"
)

// Android has no service manager wanctl can install into. systemd, launchd and
// the Windows scheduler all let an unprivileged user register a unit that the
// OS brings back; Android deliberately has no such thing for a non-root process
// — the platform decides what runs, and a plain background process is subject
// to being killed under memory pressure and is gone after a reboot.
//
// The two Android environments each have their own answer, and neither is
// something `wanctl service install` can perform on the user's behalf, so this
// returns an error that says what to actually do rather than reporting a
// success it did not achieve.
func androidServiceUnsupported(self string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "`wanctl service` cannot install a service on Android: the platform has no user-installable service manager.\n\n")
	if prefix := strings.TrimSpace(os.Getenv("PREFIX")); prefix != "" {
		fmt.Fprintf(&b, "You are in Termux. To keep the agent alive there:\n")
		fmt.Fprintf(&b, "  1. termux-wake-lock            # stop Android from dozing the process\n")
		fmt.Fprintf(&b, "  2. %s start                   # detached agent\n", self)
		fmt.Fprintf(&b, "  Restart on boot needs the separate Termux:Boot app, whose ~/.termux/boot/\n")
		fmt.Fprintf(&b, "  scripts run at startup; `pkg install termux-services` adds a runit supervisor.\n")
		fmt.Fprintf(&b, "  (Neither has been verified by the wanctl maintainers — see docs/android.md.)\n")
	} else {
		fmt.Fprintf(&b, "Run it detached instead:\n  %s start\n\n", self)
		fmt.Fprintf(&b, "Note the limits of that outside Termux: the process does not survive a reboot,\n")
		fmt.Fprintf(&b, "and Android may kill it under memory pressure. For an agent that comes back,\n")
		fmt.Fprintf(&b, "run it from Termux with a wake lock (see docs/android.md).\n")
	}
	return fmt.Errorf("%s", b.String())
}
