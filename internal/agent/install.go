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
	Version   string
	Unchanged func() bool // binary identity check, called with installMu held
	Restart   func()
	Check     func(current, target string) error
	Run       func(context.Context, string, string, func(string)) (string, error)
}

func (a *Agent) ConfigureInstall(h InstallHooks) {
	a.installMu.Lock()
	defer a.installMu.Unlock()
	a.installer = h
	if a.installVersion == "" {
		a.installVersion = h.Version
	}
}

var ErrRestartPending = errors.New("an update was just installed; the agent is restarting")

// Must be called with installMu held. An external CLI update changes the file
// identity even though this old process still reports its original build.
func (a *Agent) installReadyLocked() error {
	if a.installer.Unchanged != nil && !a.installer.Unchanged() {
		a.restartPending = true
	}
	if a.restartPending {
		return ErrRestartPending
	}
	return nil
}

// InstallAutoUpdate owns the same transaction as a web install. Success makes
// restart-pending irreversible BEFORE releasing the lock, not later in tick.
func (a *Agent) InstallAutoUpdate(ctx context.Context, install func(context.Context, string) (string, error)) (string, error) {
	resume := a.PrepareDesktopUpdate()
	defer resume()
	if err := a.installReadyLocked(); err != nil {
		return "", err
	}
	version, err := install(ctx, a.installVersion)
	if err == nil {
		a.installVersion = version
		a.restartPending = true
	}
	return version, err
}

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
		return protocol.Message{Kind: protocol.KindError, Reason: installFailureReason(err)}, nil
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
	if err := a.installReadyLocked(); err != nil {
		return refuse(err)
	}
	if err := validateInstall(m.Package, m.Version, runtime.GOOS); err != nil {
		return refuse(err)
	}
	if a.Busy() {
		return refuse(errors.New("device is busy with a session, job, desktop call or approval; try again when it finishes"))
	}
	if m.Package == "wanctl" {
		if a.installer.Check == nil || a.installer.Run == nil {
			return refuse(errors.New("this agent cannot restart after a web upgrade; run wanctl update on the device"))
		}
		if err := a.installer.Check(a.installVersion, m.Version); err != nil {
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
		restart := false
		restartAgent := a.installer.Restart
		// Handover runs after releasing the reservation, with pending already set.
		// A request during a slow restart gets the pending reason, not a new job.
		defer func() {
			if restart && restartAgent != nil {
				restartAgent()
			}
		}()
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
			version, err = a.installer.Run(ctx, a.installVersion, m.Version, emit)
		} else {
			version, err = desktop.InstallOpenCU(ctx, func() { emit("installing") })
			if err == nil {
				a.refreshOpenCUVersion(version)
			}
		}
		if errors.Is(err, ErrRestartPending) {
			a.restartPending = true
		}
		if err != nil {
			p.Stage, p.Reason = "failed", err.Error()
		} else {
			p.Version = version
			if m.Package == "wanctl" {
				a.installVersion = version
				a.restartPending = true
				p.Stage = "installing"
				p.Reason = "verified binary installed; restarting agent"
			} else {
				p.Stage = "done"
			}
		}
		a.installLog(p)
		if err != nil {
			p.Reason = installFailureReason(err)
		}
		progress(p)
		restart = err == nil && m.Package == "wanctl"
	}
}

func installMessage(p protocol.InstallProgress) protocol.Message {
	data, _ := json.Marshal(p)
	return protocol.Message{Kind: protocol.KindInstallProgress, Data: data}
}

// InstallError keeps full diagnostics in the event log and a separate plain
// sentence on the console wire. Error deliberately returns the diagnostic.
type InstallError struct {
	Reason string
	Err    error
}

func (e *InstallError) Error() string { return e.Err.Error() }
func (e *InstallError) Unwrap() error { return e.Err }
func installFailureReason(err error) string {
	var failure *InstallError
	if errors.As(err, &failure) {
		return failure.Reason
	}
	return err.Error()
}
