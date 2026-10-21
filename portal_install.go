package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"

	"wanctl/internal/agent"
	"wanctl/internal/release"
)

type portalUpdateProbe struct {
	version, self, platform string
	apk                     func(string) bool
	writable                func(string) bool
}

func checkPortalUpdate(p portalUpdateProbe, target string) error {
	if err := release.ValidateVersion(target); err != nil {
		return err
	}
	if release.ValidateVersion(p.version) != nil {
		return errors.New("development builds cannot be upgraded from the web")
	}
	if p.apk(p.self) {
		return errors.New("this copy is inside the Android app; use the app's update check")
	}
	cmp, _ := release.CompareVersions(target, p.version)
	if cmp <= 0 {
		return errors.New("the requested version must be strictly newer than the running version; same versions and downgrades are refused")
	}
	if !p.writable(filepath.Dir(p.self)) {
		return fmt.Errorf("binary directory %s is not writable; run wanctl update with sufficient rights on the device", filepath.Dir(p.self))
	}
	if p.platform == "android" {
		return errors.New("this Android installation cannot restart on the new binary; run wanctl update on the device")
	}
	return nil
}

type portalUpdater struct {
	self    string
	updated atomic.Bool
}

func configurePortalUpdate(a *agent.Agent, version string, restart func()) *portalUpdater {
	self, pathErr := selfPath()
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	u := &portalUpdater{self: self}
	original, statErr := os.Stat(self)
	a.ConfigureInstall(agent.InstallHooks{
		Version: version,
		Unchanged: func() bool {
			if statErr != nil {
				return true
			} // Check reports path/permission failures.
			now, err := os.Stat(self)
			return err == nil && os.SameFile(original, now) && original.Size() == now.Size() && original.ModTime() == now.ModTime()
		},
		Check: func(current, target string) error {
			if pathErr != nil {
				return fmt.Errorf("cannot locate running binary: %w", pathErr)
			}
			return checkPortalUpdate(portalUpdateProbe{current, self, runtime.GOOS, runningFromAPK, canWriteDir}, target)
		},
		Run: func(ctx context.Context, current, target string, stage func(string)) (installed string, runErr error) {
			defer func() {
				var failure *agent.InstallError
				if runErr != nil && !errors.As(runErr, &failure) {
					reason := "wanctl installation failed; check the device logs"
					if errors.Is(runErr, agent.ErrRestartPending) {
						reason = agent.ErrRestartPending.Error()
					}
					runErr = &agent.InstallError{Reason: reason, Err: runErr}
				}
			}()
			bases, err := updateSourcesForVersion(target)
			if err != nil {
				return "", err
			}
			got, err := fetchPortalUpdate(target, bases, func(base string) (updateFetch, error) {
				path, v, err := downloadSignedVersion(ctx, base, filepath.Dir(self), runtime.GOOS, runtime.GOARCH, current, target)
				return updateFetch{path: path, version: v}, err
			})
			if err != nil {
				return "", err
			}
			defer os.Remove(got.path)
			if err := ctx.Err(); err != nil {
				return "", err
			}
			stage("installing")
			if now, err := os.Stat(self); statErr == nil && (err != nil || !os.SameFile(original, now) || original.Size() != now.Size() || original.ModTime() != now.ModTime()) {
				return "", agent.ErrRestartPending
			}
			if err := os.Chmod(got.path, 0755); err != nil {
				return "", err
			}
			if err := replaceBinary(got.path, self); err != nil {
				return "", err
			}
			u.updated.Store(true)
			return got.version, nil
		},
		Restart: restart,
	})
	return u
}
