package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"time"

	"wanctl/internal/desktop"
	"wanctl/internal/eventlog"
	"wanctl/internal/protocol"
	"wanctl/internal/release"
)

// InstallHooks binds the command's signed updater to the agent control plane.
// Configure it before Run. Check must not download; Run owns the swap/restart.
type InstallHooks struct {
	Restart func()
	Check   func(string) error
	Run     func(context.Context, string, func(string)) (string, error)
}

func (a *Agent) ConfigureInstall(h InstallHooks) { a.installer = h }

func validateInstall(pkg, version, platform string) error {
	switch pkg {
	case "wanctl":
		if len(version) > 128 {
			return errors.New("version is too long")
		}
		return release.ValidateVersion(version)
	case "opencu":
		if platform != "windows" {
			return errors.New("OpenCU installation requires Windows")
		}
		if version != "latest" {
			return errors.New("OpenCU only accepts version latest")
		}
		return nil
	default:
		return errors.New("only wanctl and opencu can be installed")
	}
}

func (a *Agent) installLog(p protocol.InstallProgress) {
	if a.log != nil {
		_ = a.log.Append(eventlog.Event{Type: "install", Detail: fmt.Sprintf("%s %s: %s", p.Package, p.Version, p.Reason), Decision: p.Stage})
	}
}

// beginInstall reserves the device before acknowledging. The work survives the
// requesting console's disconnect, but is joined/cancelled by Agent.Close.
func (a *Agent) beginInstall(m protocol.Message, progress func(protocol.InstallProgress)) (protocol.Message, func()) {
	p := protocol.InstallProgress{Package: m.Package, Version: m.Version, Stage: "requested"}
	a.installLog(p)
	refuse := func(err error) (protocol.Message, func()) {
		p.Stage, p.Reason = "failed", err.Error()
		a.installLog(p)
		return protocol.Message{Kind: protocol.KindError, Reason: p.Reason}, nil
	}
	if err := validateInstall(m.Package, m.Version, runtime.GOOS); err != nil {
		return refuse(err)
	}
	if !a.installMu.TryLock() {
		return refuse(errors.New("an install is already running"))
	}
	unlock := true
	defer func() {
		if unlock {
			a.installMu.Unlock()
		}
	}()
	if a.Busy() {
		return refuse(errors.New("device is busy with a session, job, desktop call or approval; try again when it finishes"))
	}
	if m.Package == "wanctl" {
		if a.installer.Check == nil || a.installer.Run == nil {
			return refuse(errors.New("this agent cannot restart after a web upgrade; run wanctl update on the device"))
		}
		if err := a.installer.Check(m.Version); err != nil {
			return refuse(err)
		}
	}
	// TryLock closes the gap between the busy probe and taking the desktop lock.
	resume, ok := a.desktop.TryPauseForInstall()
	if !ok {
		return refuse(errors.New("desktop is busy; try again when the current call finishes"))
	}
	unlock = false
	return protocol.Message{Kind: protocol.KindPackageInstall}, func() {
		defer a.installMu.Unlock()
		defer resume()
		ctx, cancel := context.WithTimeout(a.shutdown(), 8*time.Minute)
		defer cancel()
		emit := func(stage string) { p.Stage = stage; progress(p) }
		emit("downloading")
		a.desktop.CloseIdleOpenCU()
		var version string
		var err error
		if m.Package == "wanctl" {
			version, err = a.installer.Run(ctx, m.Version, emit)
		} else {
			version, err = desktop.InstallOpenCU(ctx, func() { emit("installing") })
			if err == nil {
				a.refreshOpenCUVersion(version)
			}
		}
		if err != nil {
			p.Stage, p.Reason = "failed", err.Error()
		} else {
			p.Version = version
			if m.Package == "wanctl" {
				p.Stage = "installing"
				p.Reason = "verified binary installed; restarting agent"
			} else {
				p.Stage = "done"
			}
		}
		a.installLog(p)
		progress(p)
		if err == nil && m.Package == "wanctl" && a.installer.Restart != nil {
			a.installer.Restart()
		}
	}
}

func installMessage(p protocol.InstallProgress) protocol.Message {
	data, _ := json.Marshal(p)
	return protocol.Message{Kind: protocol.KindInstallProgress, Data: data}
}
