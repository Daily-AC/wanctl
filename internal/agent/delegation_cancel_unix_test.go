//go:build !windows

package agent

import (
	"io"
	"syscall"
	"testing"
	"time"
	"wanctl/internal/client"
	"wanctl/internal/policy"
)

func TestDelegatedActiveCommandsStopOnRevokeAndExpiry(t *testing.T) {
	for _, agentTransport := range []string{"ws", "http"} {
		for _, controllerTransport := range []string{"ws", "http"} {
			for _, revoke := range []bool{false, true} {
				name := agentTransport + "-" + controllerTransport + "-" + map[bool]string{false: "expiry", true: "revoke"}[revoke]
				t.Run(name, func(t *testing.T) {
					ttl := 3 * time.Second
					if revoke {
						ttl = time.Minute
					}
					f := startDelegationFixture(t, agentTransport, controllerTransport, policy.ModeBypass, true, ttl)
					command, pidFile := remoteProbe(t)
					done := make(chan error, 1)
					go func() {
						_, err := f.c.ExecTo(f.ctx, client.ExecRequest{Target: f.target, Command: command, OneShot: true}, io.Discard, io.Discard)
						done <- err
					}()
					pid := waitForRemotePID(t, pidFile)
					t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
					if revoke {
						f.tokens.revoke()
					}
					select {
					case err := <-done:
						if err == nil {
							t.Fatal("revoked/expired command reported success")
						}
					case <-time.After(6 * time.Second):
						t.Fatal("command still connected")
					}
					if !remoteGone(pid, 3*time.Second) {
						t.Fatal("revoked/expired command process survived")
					}
				})
			}
		}
	}
}
