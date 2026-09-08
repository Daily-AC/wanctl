//go:build !android

package transport

import "os"

func publishDeviceID(temp, path string) error {
	return os.Link(temp, path)
}
