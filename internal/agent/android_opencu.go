package agent

import (
	"context"
	"errors"

	"wanctl/internal/desktop"
	"wanctl/internal/opencu"
)

func (a *Agent) configureAndroidOpenCU(goos string, getenv func(string) string) {
	if goos != "android" {
		return
	}
	dir := opencu.AndroidFilesDir(goos, getenv)
	if dir == "" {
		a.openCUStart = func(context.Context) (desktop.OpenCUConnection, error) {
			return nil, errors.New(opencu.AndroidNeedsApp)
		}
		return
	}
	cache := opencu.NewAndroidCache(dir, getenv(opencu.AndroidURLEnv))
	a.openCUVersion = &installedVersionCache{read: cache.CachedVersion}
	host := &desktop.AndroidOpenCU{Cache: cache, Elevator: a.elevator, OnVersion: a.refreshOpenCUVersion}
	a.openCUStart = host.StartOpenCU
}
