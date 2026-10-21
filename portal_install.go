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
	restart                 func() error
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
	return p.restart()
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
	a.ConfigureInstall(agent.InstallHooks{
		Check: func(target string) error {
			if pathErr != nil {
				return fmt.Errorf("cannot locate running binary: %w", pathErr)
			}
			return checkPortalUpdate(portalUpdateProbe{version, self, runtime.GOOS, runningFromAPK, canWriteDir, portalRestartSupported}, target)
		},
		Run: func(ctx context.Context, target string, stage func(string)) (string, error) {
			bases, err := updateSourcesForVersion(target)
			if err != nil {
				return "", err
			}
			got, err := overSources(bases, func(base string) (updateFetch, error) {
				path, v, err := downloadSignedVersion(ctx, base, filepath.Dir(self), runtime.GOOS, runtime.GOARCH, version, target)
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

// The Windows managed flag alone says nothing about whether a wrapper restarts
// after a clean exit. Only our parent loop makes that promise. Unix exec keeps
// the supervisor's original PID, so no wrapper behaviour is assumed there.
func checkPortalRestartMode(platform string, managed, supervised, sessionZero bool) error {
	if platform != "windows" {
		return nil
	}
	if managed {
		if supervised {
			return nil
		}
		return errors.New("this Windows supervisor is not known to restart the new binary; run wanctl update on the device and restart its service or task")
	}
	if sessionZero {
		return errors.New("this session-0 agent has no verified restart supervisor; run wanctl update on the device and restart its service or task")
	}
	return nil
}
