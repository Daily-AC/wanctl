package agent

import (
	"context"
	"runtime"
	"sync"
	"time"

	"wanctl/internal/opencu"
	"wanctl/internal/versionmeta"
)

const versionRefreshInterval = time.Minute

type installedVersionCache struct {
	once  sync.Once
	mu    sync.RWMutex
	value string
	read  func() string
}

func newInstalledVersionCache() *installedVersionCache {
	if runtime.GOOS != "windows" {
		return nil
	}
	return &installedVersionCache{read: opencu.InstalledVersion}
}

func (c *installedVersionCache) start(ctx context.Context) {
	if c == nil {
		return
	}
	c.once.Do(func() {
		// Deliberately not joined by Agent.Close: OS file/profile I/O is not
		// cancellable. At most this ONE metadata-only reader can outlive shutdown;
		// it never writes agent configuration or starts a replacement reader.
		go func() {
			for ctx.Err() == nil {
				value := c.read()
				if ctx.Err() != nil {
					return
				}
				c.mu.Lock()
				c.value = value
				c.mu.Unlock()
				if !sleepCtx(ctx, versionRefreshInterval) {
					return
				}
			}
		}()
	})
}

func (a *Agent) versionFields() map[string]string {
	fields := map[string]string{"wanctl_version": versionmeta.Clean(a.opts.Version)}
	if c := a.openCUVersion; c != nil {
		c.mu.RLock()
		value := c.value
		c.mu.RUnlock()
		fields["opencu_version"] = versionmeta.Clean(value)
	}
	return fields
}
