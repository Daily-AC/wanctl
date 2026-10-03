//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The small COM surface below follows the Windows SDK UIAutomationClient.h.
// No provider text or COM error description is included in errors/logs.
var (
	ole32    = windows.NewLazySystemDLL("ole32.dll")
	oleaut32 = windows.NewLazySystemDLL("oleaut32.dll")
)

type automationObject struct{ table *[85]uintptr }
type automationError uint32

func (e automationError) Error() string {
	return fmt.Sprintf("UI Automation text operation failed (0x%08x); input may be partial; do not replay", uint32(e))
}
func automationHRESULT(hr uintptr) error {
	if int32(hr) < 0 {
		return automationError(uint32(hr))
	}
	return nil
}
func (o *automationObject) address() uintptr { return uintptr(unsafe.Pointer(o)) }
func (o *automationObject) release() {
	if o != nil {
		syscall.SyscallN(o.table[2], o.address()) // IUnknown::Release
	}
}

type textAutomation struct {
	client *automationObject
	check  func() error
}

func newTextAutomation(check func() error) (*textAutomation, error) {
	if err := check(); err != nil {
		return nil, err
	}
	hr, _, _ := ole32.NewProc("CoInitializeEx").Call(0, 0) // MTA, on helper's locked thread
	if err := automationHRESULT(hr); err != nil {
		return nil, err
	}
	class, _ := windows.GUIDFromString("{e22ad333-b25f-460c-83d0-0581107395c9}") // CUIAutomation8
	iid, _ := windows.GUIDFromString("{34723aff-0c9d-49d0-9896-7ab52df8cd8a}")   // IUIAutomation2
	u := &textAutomation{check: check}
	hr, _, _ = ole32.NewProc("CoCreateInstance").Call(uintptr(unsafe.Pointer(&class)), 0, 1, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&u.client)))
	if err := automationHRESULT(hr); err != nil {
		u.close()
		return nil, err
	}
	// Never let a pattern operation focus a different window. Bound every
	// provider call and check cancellation on both sides of each call.
	for _, setting := range []struct{ method, value uintptr }{{59, 0}, {61, 100}, {63, 100}} {
		if err := u.call(u.client, setting.method, setting.value); err != nil {
			u.close()
			return nil, err
		}
	}
	return u, nil
}
func (u *textAutomation) close() {
	u.client.release()
	ole32.NewProc("CoUninitialize").Call()
}

//go:uintptrescapes
func (u *textAutomation) call(o *automationObject, method uintptr, args ...uintptr) error {
	if err := u.check(); err != nil {
		return err
	}
	if o == nil {
		return errTextUnsupported
	}
	hr, _, _ := syscall.SyscallN(o.table[method], append([]uintptr{o.address()}, args...)...)
	if err := u.check(); err != nil {
		return err
	}
	return automationHRESULT(hr)
}

//go:uintptrescapes
func (u *textAutomation) object(o *automationObject, method uintptr, args ...uintptr) (*automationObject, error) {
	var result *automationObject
	var err error
	// Keep the output pointer conversion in the annotated call expression so
	// Go pins its lifetime/address across the native call (not inside append).
	switch len(args) {
	case 0:
		err = u.call(o, method, uintptr(unsafe.Pointer(&result)))
	case 1:
		err = u.call(o, method, args[0], uintptr(unsafe.Pointer(&result)))
	case 2:
		err = u.call(o, method, args[0], args[1], uintptr(unsafe.Pointer(&result)))
	case 3:
		err = u.call(o, method, args[0], args[1], args[2], uintptr(unsafe.Pointer(&result)))
	default:
		return nil, errTextUnsupported
	}
	if err != nil {
		result.release()
		return nil, err
	}
	if result == nil {
		return nil, errTextUnsupported
	}
	return result, nil
}
func (u *textAutomation) integer(o *automationObject, method uintptr) (int32, error) {
	var result int32
	err := u.call(o, method, uintptr(unsafe.Pointer(&result)))
	return result, err
}

//go:uintptrescapes
func (u *textAutomation) text(o *automationObject, method uintptr, args ...uintptr) (string, error) {
	var result *uint16
	var err error
	if len(args) == 0 {
		err = u.call(o, method, uintptr(unsafe.Pointer(&result)))
	} else if len(args) == 1 {
		err = u.call(o, method, args[0], uintptr(unsafe.Pointer(&result)))
	} else {
		return "", errTextUnsupported
	}
	if result != nil {
		defer oleaut32.NewProc("SysFreeString").Call(uintptr(unsafe.Pointer(result)))
	}
	if err != nil {
		return "", err
	}
	n, _, _ := oleaut32.NewProc("SysStringLen").Call(uintptr(unsafe.Pointer(result)))
	if n > 1<<20 {
		return "", errTextUnsupported
	}
	return textNewlines(string(utf16.Decode(unsafe.Slice(result, int(n))))), nil
}
func withAutomationString(s string, f func(uintptr) error) error {
	units := utf16.Encode([]rune(s))
	var start *uint16
	if len(units) != 0 {
		start = &units[0]
	}
	bstr, _, _ := oleaut32.NewProc("SysAllocStringLen").Call(uintptr(unsafe.Pointer(start)), uintptr(len(units)))
	if bstr == 0 {
		return errors.New("cannot allocate text for addressed delivery")
	}
	defer oleaut32.NewProc("SysFreeString").Call(bstr)
	return f(bstr)
}
func (u *textAutomation) pattern(element *automationObject, id uintptr, guid string) (*automationObject, error) {
	iid, _ := windows.GUIDFromString(guid)
	p, err := u.object(element, 14, id, uintptr(unsafe.Pointer(&iid))) // GetCurrentPatternAs
	if err == automationError(0x80040204) || err == automationError(0x80004002) || errors.Is(err, errTextUnsupported) {
		return nil, nil // capability absent, before any write
	}
	return p, err
}
