//go:build !windows

package desktop

import (
	"context"
	"os/exec"
	"wanctl/internal/protocol"
)

const Supported = false

func helperProcessAttrs(*exec.Cmd) {}
func executeJob(context.Context, Job) (protocol.DesktopResult, []byte) {
	return protocol.DesktopResult{Status: "rejected", Error: protocol.DesktopWindowsOnly, FailedIndex: -1}, nil
}
