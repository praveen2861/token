package main

import (
	"syscall"
	"time"
	"unsafe"
)

const (
	cfUnicodeText  = 13
	maxClipboardCh = 300
)

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	clipSequence     = user32.NewProc("GetClipboardSequenceNumber")
	clipOpen         = user32.NewProc("OpenClipboard")
	clipClose        = user32.NewProc("CloseClipboard")
	clipData         = user32.NewProc("GetClipboardData")
	globalLock       = kernel32.NewProc("GlobalLock")
	globalUnlock     = kernel32.NewProc("GlobalUnlock")
	stringLengthW    = kernel32.NewProc("lstrlenW")
	copyMemoryFromOS = kernel32.NewProc("RtlMoveMemory")
)

// readClipboard returns short text from the clipboard. Long text is skipped unread, since it
// can't be an address.
func readClipboard() string {
	if opened, _, _ := clipOpen.Call(0); opened == 0 {
		return ""
	}
	defer clipClose.Call()
	handle, _, _ := clipData.Call(cfUnicodeText)
	if handle == 0 {
		return ""
	}
	source, _, _ := globalLock.Call(handle)
	if source == 0 {
		return ""
	}
	defer globalUnlock.Call(handle)
	length, _, _ := stringLengthW.Call(source)
	if length == 0 || length > maxClipboardCh {
		return ""
	}
	buffer := make([]uint16, length)
	copyMemoryFromOS.Call(uintptr(unsafe.Pointer(&buffer[0])), source, length*2)
	return syscall.UTF16ToString(buffer)
}

// watchClipboard checks the clipboard's change counter, and reads the text only when it moves.
func watchClipboard(onAddress func(string)) {
	last, _, _ := clipSequence.Call()
	for range time.Tick(100 * time.Millisecond) {
		current, _, _ := clipSequence.Call()
		if current == last {
			continue
		}
		last = current
		if address := addressFromClipboard(readClipboard()); address != "" {
			onAddress(address)
		}
	}
}
