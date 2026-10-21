//go:build !windows

package desktop

import "context"

func startInstallHelper(context.Context) (*helperProcess, error) { return nil, ErrWindowsOnly }
func checkInstallSession() error                                 { return ErrWindowsOnly }
