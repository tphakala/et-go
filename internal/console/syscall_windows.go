package console

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// kernel32 functions that golang.org/x/sys/windows v0.48.0 does not wrap.
var (
	modkernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procWriteConsoleInputW    = modkernel32.NewProc("WriteConsoleInputW")
	procSetConsoleCtrlHandler = modkernel32.NewProc("SetConsoleCtrlHandler")
)

// keyEventRecord is KEY_EVENT_RECORD (16 bytes).
type keyEventRecord struct {
	KeyDown         int32 // BOOL
	RepeatCount     uint16
	VirtualKeyCode  uint16
	VirtualScanCode uint16
	UnicodeChar     uint16 // union uChar; only the WCHAR member is used
	ControlKeyState uint32
}

// inputRecord is INPUT_RECORD (20 bytes). Event is a union in C; KEY_EVENT
// is the only variant this package writes, and KEY_EVENT_RECORD is one of
// the union's largest members (MOUSE_EVENT_RECORD is as large), so the
// layout matches.
type inputRecord struct {
	EventType uint16
	_         uint16 // padding before the union
	Event     keyEventRecord
}

// writeConsoleInput appends records to the console input buffer.
func writeConsoleInput(h windows.Handle, recs []inputRecord) error {
	if len(recs) == 0 {
		return nil
	}
	var written uint32
	r1, _, err := procWriteConsoleInputW.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&recs[0])),
		uintptr(len(recs)),
		uintptr(unsafe.Pointer(&written)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}

// setConsoleCtrlHandler adds (or removes) a handler created with
// windows.NewCallback.
func setConsoleCtrlHandler(handler uintptr, add bool) error {
	var a uintptr
	if add {
		a = 1
	}
	r1, _, err := procSetConsoleCtrlHandler.Call(handler, a)
	if r1 == 0 {
		return err
	}
	return nil
}
