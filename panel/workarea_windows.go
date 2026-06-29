//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

type rect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

func primaryWorkArea() (rect, bool) {
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("SystemParametersInfoW")
	const spiGetWorkArea = 0x0030
	var r rect
	ret, _, _ := proc.Call(
		uintptr(spiGetWorkArea),
		0,
		uintptr(unsafe.Pointer(&r)),
		0,
	)
	return r, ret != 0
}
