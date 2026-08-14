package agent

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"time"

	"wanctl/internal/adb"
	"wanctl/internal/transport"
)

// pairTimeout bounds the whole pairing exchange. The device's pairing window
// closes on its own, so hanging past it helps nobody.
const pairTimeout = 30 * time.Second

// runADBPair handles the `adb-pair <port> <code>` verb.
//
// Pairing runs on the DEVICE, against its own loopback, which is why this is a
// verb rather than a controller-side command: only the device can reach the
// pairing port adbd opened. It deliberately does NOT require elevation — a
// TCP connection to 127.0.0.1 is something the app sandbox can already make,
// and requiring elevation to set up the elevation channel would be a loop with
// no entry point.
//
// The port is the one the device shows under "Pair device with pairing code",
// which is NOT the port shown on the wireless-debugging screen itself; those
// are two different listeners and mixing them up is the most likely reason for
// a connection refused here.
func (a *Agent) runADBPair(command string, out io.Writer) (handled bool, code int, err error) {
	fields := strings.Fields(command)
	if len(fields) == 0 || fields[0] != "adb-pair" {
		return false, 0, nil
	}
	if runtime.GOOS != "android" {
		return true, 0, fmt.Errorf("adb-pair is only meaningful on Android")
	}
	if len(fields) != 3 {
		return true, 0, fmt.Errorf(
			"usage: adb-pair <pairing-port> <6-digit-code>\n" +
				"Both are shown on the device under 设置 → 开发者选项 → 无线调试 → " +
				"使用配对码配对设备. Use the port from THAT screen, not the one on the " +
				"wireless debugging screen — they are different listeners.")
	}
	port, perr := strconv.Atoi(fields[1])
	if perr != nil || port <= 0 || port > 65535 {
		return true, 0, fmt.Errorf("adb-pair: %q is not a valid port", fields[1])
	}
	pairCode := fields[2]
	if len(pairCode) != 6 || strings.TrimFunc(pairCode, isDigit) != "" {
		return true, 0, fmt.Errorf("adb-pair: the pairing code is six digits; got %q", pairCode)
	}

	dir, derr := transport.ConfigDir()
	if derr != nil {
		return true, 0, derr
	}
	key, kerr := adb.LoadOrCreateKey(dir, adbKeyName(a.opts.Name))
	if kerr != nil {
		return true, 0, kerr
	}

	ctx, cancel := context.WithTimeout(context.Background(), pairTimeout)
	defer cancel()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if perr := adb.Pair(ctx, addr, key, pairCode); perr != nil {
		return true, 0, perr
	}
	fmt.Fprintf(out, "paired with adbd on port %d; wanctl's key is now in this device's adb_keys.\n", port)
	fmt.Fprintf(out, "The adb elevation channel becomes available once wireless debugging is on.\n")
	return true, 0, nil
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// adbKeyName is what the device records alongside the key.
func adbKeyName(device string) string {
	if device == "" {
		return "wanctl-agent"
	}
	return "wanctl@" + device
}
