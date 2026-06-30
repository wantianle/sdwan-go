package main

import (
	"encoding/base64"
	"log"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// trayIconBase64 is a 32x32 ICO embedded as base64 (blue circle + signal bars).
const trayIconBase64 = "AAABAAEAICAAAAEAIAAsAQAAFgAAAIlQTkcNChoKAAAADUlIRFIAAAAgAAAAIAgGAAAAc3p69AAAAPNJREFUeJzcl90OwiAMhWHxqfXavTbGDE1HfyzrYZL1evb7PJQNlvTnigvcS4n8PA+DrdnV2ydggMszpfw4LmILNOA3zFtMShHRZyAAF59XUpQTIA/3gkUITaNJgicAhrM+TRLqEqDgv/rtBYJ72l2EIyaA/vdW36W1GgVnEpV3gW9B3Wbm2xAtcBQGE0DWJnDSAH6KDuImUF+PyGit+nLWnCdZgs5CLtUN0SQiNNESnDSIdACTlsAoCanvXsB5kg0X4agzgE5B68cFiB1KwjoT6pEHT8UMLMCTuQ1b0840vPeCyW9GHSK9YFwFT9KvAAAA//+M0F6mPaJNWAAAAABJRU5ErkJggg=="

var trayIconBytes = loadTrayIcon()

func loadTrayIcon() []byte {
	data, _ := base64.StdEncoding.DecodeString(trayIconBase64)
	return data
}

// trayShowCh signals the Wails window to show.
var trayShowCh = make(chan struct{}, 1)

// shutdownCh signals the Wails app to gracefully shut down.
var shutdownCh = make(chan struct{})

var shutdownOnce sync.Once

const (
	WM_USER          = 0x0400
	WM_TRAYICON      = WM_USER + 1
	WM_LBUTTONDBLCLK = 0x0203
	WM_RBUTTONUP     = 0x0205
	WM_LBUTTONUP     = 0x0202
	WM_DESTROY       = 0x0002
	IDM_EXIT         = 1
)

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	shell32                 = windows.NewLazySystemDLL("shell32.dll")
	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procShellNotifyIconW    = shell32.NewProc("Shell_NotifyIconW")
	procLoadImageW          = user32.NewProc("LoadImageW")
	procDestroyIcon         = user32.NewProc("DestroyIcon")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
)

type WNDCLASSEXW struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type POINT struct{ X, Y int32 }

type NOTIFYICONDATAW struct {
	Size        uint32
	Wnd         windows.Handle
	ID          uint32
	Flags       uint32
	CallbackMsg uint32
	Icon        windows.Handle
	Tip         [128]uint16
	State       uint32
	StateMask   uint32
	Info        [256]uint16
	Version     uint32
	InfoTitle   [64]uint16
	InfoFlags   uint32
	GuidItem    windows.GUID
	BalloonIcon windows.Handle
}

const NIF_ICON = 0x00000002
const NIF_MESSAGE = 0x00000001
const NIF_TIP = 0x00000004
const NIM_ADD = 0x00000000
const NIM_DELETE = 0x00000002
const IMAGE_ICON = 1
const LR_LOADFROMFILE = 0x00000010
const MF_STRING = 0x00000000

var (
	trayWnd     windows.Handle = 0
	trayHIcon   windows.Handle = 0
	icoTempPath string
)

type MSG struct {
	HWnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      POINT
}

func startSysTray() {
	// Write ICO to temp file (LoadImageW needs a file path)
	f, err := os.CreateTemp("", "sdwan-*.ico")
	if err != nil {
		log.Printf("[TRAY] Cannot create temp icon: %v", err)
		return
	}
	f.Write(trayIconBytes)
	f.Close()
	icoTempPath = f.Name()
	defer os.Remove(icoTempPath)

	// Load icon from file
	pathPtr, _ := windows.UTF16PtrFromString(icoTempPath)
	hicon, _, _ := procLoadImageW.Call(
		0,
		uintptr(unsafe.Pointer(pathPtr)),
		IMAGE_ICON,
		0, 0,
		LR_LOADFROMFILE,
	)
	if hicon == 0 {
		log.Printf("[TRAY] LoadImageW failed")
		return
	}
	trayHIcon = windows.Handle(hicon)
	defer procDestroyIcon.Call(uintptr(trayHIcon))

	// Register window class
	className, _ := windows.UTF16PtrFromString("SDWANTrayClass2")
	instance, _, _ := windows.NewLazyDLL("kernel32.dll").NewProc("GetModuleHandleW").Call(0)

	wc := WNDCLASSEXW{
		Size:      uint32(unsafe.Sizeof(WNDCLASSEXW{})),
		Style:     0,
		WndProc:   syscall.NewCallback(trayWndProc),
		Instance:  windows.Handle(instance),
		ClassName: className,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	// Create hidden message-only window
	result, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("SDWAN Tray"))),
		0,
		0, 0, 0, 0,
		0,
		0,
		uintptr(instance),
		0,
	)
	trayWnd = windows.Handle(result)
	if trayWnd == 0 {
		log.Printf("[TRAY] CreateWindow failed")
		return
	}

	// Add tray icon
	addTrayIcon()

	// Message loop
	var msg MSG
	for {
		ret, _, _ := procGetMessageW.Call(
			uintptr(unsafe.Pointer(&msg)),
			0, 0, 0,
		)
		if ret == 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	log.Println("[TRAY] Message loop exited")
}

func addTrayIcon() {
	title, _ := windows.UTF16FromString("SDWAN Panel")
	nid := NOTIFYICONDATAW{
		Size:        uint32(unsafe.Sizeof(NOTIFYICONDATAW{})),
		Wnd:         trayWnd,
		ID:          100,
		Flags:       NIF_ICON | NIF_MESSAGE | NIF_TIP,
		CallbackMsg: WM_TRAYICON,
		Icon:        trayHIcon,
	}
	copy(nid.Tip[:], title)
	procShellNotifyIconW.Call(NIM_ADD, uintptr(unsafe.Pointer(&nid)))
	log.Println("[TRAY] Tray icon added")
}

func deleteTrayIcon() {
	if trayWnd == 0 {
		return
	}
	nid := NOTIFYICONDATAW{
		Size: uint32(unsafe.Sizeof(NOTIFYICONDATAW{})),
		Wnd:  trayWnd,
		ID:   100,
	}
	procShellNotifyIconW.Call(NIM_DELETE, uintptr(unsafe.Pointer(&nid)))
	log.Println("[TRAY] Tray icon removed")
}

func trayWndProc(hwnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_TRAYICON:
		switch lParam {
		case WM_LBUTTONUP:
			log.Println("[TRAY] Left click: show panel")
			signalTrayShow()
			return 0
		case WM_LBUTTONDBLCLK:
			log.Println("[TRAY] Left double click: show panel")
			signalTrayShow()
			return 0
		case WM_RBUTTONUP:
			// Right-click: show context menu with just "退出"
			procSetForegroundWindow.Call(uintptr(hwnd))
			hMenu, _, _ := procCreatePopupMenu.Call()
			exitStr, _ := windows.UTF16PtrFromString("退出 SDWAN Panel")
			procAppendMenuW.Call(hMenu, MF_STRING, IDM_EXIT, uintptr(unsafe.Pointer(exitStr)))
			var pt POINT
			procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
			cmd, _, _ := procTrackPopupMenu.Call(
				hMenu,
				0x0100, // TPM_RETURNCMD | TPM_LEFTALIGN
				uintptr(pt.X), uintptr(pt.Y),
				0, uintptr(hwnd), 0,
			)
			if cmd == IDM_EXIT {
				deleteTrayIcon()
				procDestroyWindow.Call(uintptr(hwnd))
				procPostQuitMessage.Call(0)
				shutdownOnce.Do(func() { close(shutdownCh) })
			}
			return 0
		}
	case WM_DESTROY:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return ret
}

func signalTrayShow() {
	select {
	case trayShowCh <- struct{}{}:
	default:
	}
}
