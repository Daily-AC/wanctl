//go:build !windows

package desktop

import (
	"context"
	"os/exec"
	"wanctl/internal/protocol"
)

const Supported = false

func helperProcessAttrs(*exec.Cmd) {}
func SessionZero() (bool, error)   { return false, nil }
func CheckCaptureSession() error   { return nil }
func PipeHelperMain(string) int    { return 1 }
func runPlatformHelper(ctx context.Context, job Job) (protocol.DesktopResult, []byte, error) {
	return exchangeHelper(ctx, job, startInheritedHelper)
}
func executeJob(context.Context, Job) (protocol.DesktopResult, []byte) {
	return protocol.DesktopResult{Status: "rejected", Error: protocol.DesktopWindowsOnly, FailedIndex: -1}, nil
}
