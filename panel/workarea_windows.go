//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

type point struct {
	X int32
	Y int32
}


type workArea struct {
	MonitorLeft   int32
	MonitorTop    int32
	MonitorRight  int32
	MonitorBottom int32
	WorkLeft      int32
	WorkTop       int32
	WorkRight     int32
	WorkBottom    int32
	MonitorWidth  int32
	MonitorHeight int32
}

type rect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type monitorInfo struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect
	DwFlags   uint32
}

func primaryWorkArea() (workArea, bool) {
	user32 := syscall.NewLazyDLL("user32.dll")
	monitorFromPoint := user32.NewProc("MonitorFromPoint")
	getMonitorInfo := user32.NewProc("GetMonitorInfoW")
	const monitorDefaultToPrimary = 1

	pt := point{X: 0, Y: 0}
	monitor, _, _ := monitorFromPoint.Call(
		*(*uintptr)(unsafe.Pointer(&pt)),
		uintptr(monitorDefaultToPrimary),
	)
	if monitor == 0 {
		return workArea{}, false
	}

	info := monitorInfo{CbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
	ret, _, _ := getMonitorInfo.Call(
		monitor,
		uintptr(unsafe.Pointer(&info)),
	)
	if ret == 0 {
		return workArea{}, false
	}

	return workArea{
		MonitorLeft:   info.RcMonitor.Left,
		MonitorTop:    info.RcMonitor.Top,
		MonitorRight:  info.RcMonitor.Right,
		MonitorBottom: info.RcMonitor.Bottom,
		WorkLeft:      info.RcWork.Left,
		WorkTop:       info.RcWork.Top,
		WorkRight:     info.RcWork.Right,
		WorkBottom:    info.RcWork.Bottom,
		MonitorWidth:  info.RcMonitor.Right - info.RcMonitor.Left,
		MonitorHeight: info.RcMonitor.Bottom - info.RcMonitor.Top,
	}, true
}
