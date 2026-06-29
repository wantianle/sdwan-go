//go:build !windows

package main

type rect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

func primaryWorkArea() (rect, bool) {
	return rect{}, false
}
