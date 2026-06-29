//go:build !windows

package main

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

func primaryWorkArea() (workArea, bool) {
	return workArea{}, false
}
