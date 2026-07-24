package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"wanctl/internal/config"
)

// cmdSupervise is the restart loop used by the Windows Scheduled Task, whose
// native ONLOGON trigger does not restart a process that exits later. Each
// iteration executes the stable binary path again, so a replacement is picked
// up on the next child start.
func cmdSupervise(ctx context.Context, args []string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	agentArgs := append([]string{"agent", "--managed"}, args...)
	for {
		cmd := exec.CommandContext(ctx, self, agentArgs...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "wanctl supervisor: agent exited: %v; restarting in 3s\n", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// cmdUp is the bare-`wanctl` entrypoint: enroll over OAuth if we have no token
// yet, then make sure the agent is running in the background. Idempotent.
func cmdUp(ctx context.Context) error {
	tok := config.EnvOr("WANCTL_TOKEN", config.StoredToken())
	if tok == "" {
		t, err := enroll(ctx)
		if err != nil {
			return err
		}
		if err := config.SaveToken(t); err != nil {
			return fmt.Errorf("save token: %w", err)
		}
	}
	return cmdStart()
}

// cmdStart launches the agent detached in the background and records its pid.
func cmdStart() error {
	if pid := config.ReadPID(); processAlive(pid) {
		fmt.Printf("wanctl 服务已在运行 (pid %d)。停止用: wanctl stop\n", pid)
		return nil
	}
	if config.EnvOr("WANCTL_TOKEN", config.StoredToken()) == "" {
		return fmt.Errorf("尚未登录：先运行 `wanctl`（无参）完成飞书授权")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logPath, err := config.LogPath()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer logf.Close()

	// The child reads token from <cfg>/token and relay/transport from compile-time
	// defaults, so no flags are needed. detachSysProcAttr() detaches it from this
	// terminal so it survives the parent exiting.
	cmd := exec.Command(self, "agent")
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = detachSysProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}
	pid := cmd.Process.Pid // capture before Release() zeroes it
	if err := config.WritePID(pid); err != nil {
		return fmt.Errorf("write pid: %w", err)
	}
	_ = cmd.Process.Release() // detach: don't reap; it runs until `wanctl stop`
	fmt.Printf("✓ 服务已转后台 (pid %d)，日志: %s\n  停止用: wanctl stop\n", pid, logPath)
	return nil
}

// cmdStop terminates the background agent.
func cmdStop() error {
	pid := config.ReadPID()
	if !processAlive(pid) {
		_ = config.RemovePID()
		fmt.Println("wanctl 服务未在运行")
		return nil
	}
	if err := terminatePID(pid); err != nil {
		return fmt.Errorf("stop pid %d: %w", pid, err)
	}
	_ = config.RemovePID()
	fmt.Printf("✓ 已停止 wanctl 服务 (pid %d)\n", pid)
	return nil
}

// cmdStatus reports whether the background agent is running.
func cmdStatus() error {
	if pid := config.ReadPID(); processAlive(pid) {
		fmt.Printf("● 运行中 (pid %d)\n", pid)
	} else {
		fmt.Println("○ 未运行（运行 `wanctl` 启动）")
	}
	if config.StoredToken() != "" {
		fmt.Println("  凭证: 已登录")
	} else if os.Getenv("WANCTL_TOKEN") != "" {
		fmt.Println("  凭证: 来自 WANCTL_TOKEN 环境变量")
	} else {
		fmt.Println("  凭证: 未登录")
	}
	return nil
}

// cmdLogout stops the agent and clears the stored token.
func cmdLogout() error {
	_ = cmdStop()
	if err := config.ClearToken(); err != nil {
		return err
	}
	fmt.Println("✓ 已登出（已清除本地凭证）")
	return nil
}
