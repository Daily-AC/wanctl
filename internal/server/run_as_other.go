//go:build !windows

package server

import (
	"context"
	"fmt"
	"io"
)

func RunAsUserContext(ctx context.Context, shell, command, cwd, username string, out io.Writer) (int, error) {
	return -1, fmt.Errorf("--as is only supported by Windows agents running as SYSTEM")
}
