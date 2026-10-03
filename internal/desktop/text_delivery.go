package desktop

import "wanctl/internal/protocol"

// Keep the resolved receiver in the delivery call. A last-moment focus check
// can reduce the race, but cannot make a global input queue target-specific.
// Even if focus moves after resolution, delivery must retain this address.
func addressedUnicode(expected protocol.DesktopWindow, unit uint16, down bool, check func() error, resolve func(protocol.DesktopWindow) (uintptr, error), send func(uintptr, uint16) error) error {
	if !down {
		return nil
	} // addressed characters do not hold global key state
	if err := check(); err != nil {
		return err
	}
	target, err := resolve(expected)
	if err != nil {
		return err
	}
	if err = check(); err != nil {
		return err
	}
	return send(target, unit)
}
