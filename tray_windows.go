//go:build windows

package main

import (
	_ "embed"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

//go:embed assets/tray.png
var trayPNG []byte

const (
	wmDestroy         = 0x0002
	wmClose           = 0x0010
	wmQueryEndSession = 0x0011
	wmEndSession      = 0x0016
	wmCommand         = 0x0111
	wmLButtonUp       = 0x0202
	wmLButtonDblClk   = 0x0203
	wmRButtonUp       = 0x0205
	wmClipboardUpdate = 0x031D
	wmApp             = 0x8000
	wmTray            = wmApp + 1
	ninBalloonClick   = 0x0405

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10

	niifInfo  = 0x01
	niifError = 0x03

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfChecked   = 0x0008
	mfSeparator = 0x0800

	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020
	tpmNoNotify    = 0x0080
	tpmReturnCmd   = 0x0100
)

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type winMsg struct {
	hwnd     uintptr
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       struct{ x, y int32 }
	lPrivate uint32
}

type notifyIconData struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         guid
	hBalloonIcon     uintptr
}

var tray struct {
	mu             sync.Mutex
	hwnd           uintptr
	icon           uintptr
	tip            string
	lastNotifyPath string
	taskbarCreated uint32
}

func (n *notifyIconData) init() {
	n.cbSize = uint32(unsafe.Sizeof(*n))
	n.hWnd = tray.hwnd
	n.uID = 1
}

func loadTrayIcon(hInst uintptr) uintptr {
	cx, _, _ := pGetSystemMetrics.Call(49) // SM_CXSMICON
	cy, _, _ := pGetSystemMetrics.Call(50)
	// Icon resource #1 (from the .syso), else the embedded PNG.
	if h, _, _ := pLoadImageW.Call(hInst, 1, 1, cx, cy, 0); h != 0 {
		return h
	}
	if len(trayPNG) > 0 {
		h, _, _ := pCreateIconFromResourceEx.Call(uintptr(unsafe.Pointer(&trayPNG[0])), uintptr(len(trayPNG)), 1, 0x00030000, cx, cy, 0)
		if h != 0 {
			return h
		}
	}
	h, _, _ := pLoadImageW.Call(0, 32512, 1, 0, 0, 0x8000) // IDI_APPLICATION, LR_SHARED
	return h
}

func addTrayIcon() {
	var nid notifyIconData
	nid.init()
	nid.uFlags = nifMessage | nifIcon | nifTip
	nid.uCallbackMessage = wmTray
	nid.hIcon = tray.icon
	tray.mu.Lock()
	tip := tray.tip
	tray.mu.Unlock()
	if tip == "" {
		tip = T("tipIdle")
	}
	copyU16(nid.szTip[:], tip)
	pShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
}

// runTray creates the hidden window + tray icon and runs the message loop until Quit.
func runTray(onReady func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hInst, _, _ := pGetModuleHandleW.Call(0)
	className := u16("MCDownloadManagerTray")
	wc := wndClassEx{lpfnWndProc: syscall.NewCallback(wndProc), hInstance: hInst, lpszClassName: className}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	hwnd, _, _ := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(u16(appName))),
		0, 0, 0, 0, 0, 0, 0, hInst, 0)
	if hwnd == 0 {
		return
	}
	tc, _, _ := pRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(u16("TaskbarCreated"))))
	tray.mu.Lock()
	tray.hwnd = hwnd
	tray.taskbarCreated = uint32(tc)
	tray.mu.Unlock()
	tray.icon = loadTrayIcon(hInst)
	addTrayIcon()
	pAddClipboardFormatListener.Call(hwnd)
	refreshAutostartPath()
	if !appSettings.Get().AutostartOffered {
		_ = setAutostart(true)
		appSettings.Update(func(s *Settings) { s.AutostartOffered = true })
	}

	go onReady()

	var m winMsg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func quitTray() {
	tray.mu.Lock()
	h := tray.hwnd
	tray.mu.Unlock()
	pPostMessageW.Call(h, wmClose, 0, 0)
}

func wndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch uint32(msg) {
	case wmTray:
		switch uint32(lParam) {
		case wmLButtonUp, wmLButtonDblClk:
			go openUI()
		case wmRButtonUp:
			showMenu(hwnd)
		case ninBalloonClick:
			tray.mu.Lock()
			p := tray.lastNotifyPath
			tray.mu.Unlock()
			if p != "" && fileExists(p) {
				go revealInExplorer(p)
			} else {
				go openUI()
			}
		}
		return 0
	case wmClipboardUpdate:
		onClipboardChanged()
		return 0
	case wmQueryEndSession:
		return 1
	case wmEndSession:
		if wParam != 0 {
			appStore.Shutdown()
		}
		return 0
	case wmClose:
		pDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		var nid notifyIconData
		nid.init()
		pShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
		pPostQuitMessage.Call(0)
		return 0
	}
	if tc := tray.taskbarCreated; tc != 0 && uint32(msg) == tc {
		addTrayIcon() // Explorer restarted
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

const (
	cmdOpen = iota + 1
	cmdPauseAll
	cmdResumeAll
	cmdFolder
	cmdSpeed
	cmdClipboard
	cmdAutostart
	cmdQuit
)

func showMenu(hwnd uintptr) {
	st := appSettings.Get()
	m, _, _ := pCreatePopupMenu.Call()
	add := func(id int, text string, flags uintptr) {
		pAppendMenuW.Call(m, flags, uintptr(id), uintptr(unsafe.Pointer(u16(text))))
	}
	chk := func(b bool) uintptr {
		if b {
			return mfChecked
		}
		return 0
	}
	add(cmdOpen, T("trayOpen"), mfString)
	pAppendMenuW.Call(m, mfSeparator, 0, 0)
	add(cmdPauseAll, T("trayPauseAll"), mfString)
	add(cmdResumeAll, T("trayResumeAll"), mfString)
	add(cmdFolder, T("trayFolder"), mfString)
	pAppendMenuW.Call(m, mfSeparator, 0, 0)
	if st.SpeedLimitKBps > 0 {
		add(cmdSpeed, Tf("traySpeed", fmtSize(int64(st.SpeedLimitKBps)*1024)+"/s"), mfString|chk(st.SpeedLimitOn))
	} else {
		add(cmdSpeed, T("traySpeedNone"), mfString|mfGrayed)
	}
	add(cmdClipboard, T("trayClipboard"), mfString|chk(st.WatchClipboard))
	add(cmdAutostart, T("trayAutostart"), mfString|chk(autostartEnabled()))
	pAppendMenuW.Call(m, mfSeparator, 0, 0)
	add(cmdQuit, T("trayQuit"), mfString)
	pSetMenuDefaultItem.Call(m, cmdOpen, 0)

	var pt struct{ x, y int32 }
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWindow.Call(hwnd)
	cmd, _, _ := pTrackPopupMenu.Call(m, tpmRightButton|tpmBottomAlign|tpmReturnCmd|tpmNoNotify, uintptr(pt.x), uintptr(pt.y), 0, hwnd, 0)
	pDestroyMenu.Call(m)
	pPostMessageW.Call(hwnd, 0, 0, 0) // WM_NULL: lets the menu close properly

	switch int(cmd) {
	case cmdOpen:
		go openUI()
	case cmdPauseAll:
		go appStore.PauseAll()
	case cmdResumeAll:
		go appStore.ResumeAll()
	case cmdFolder:
		d := st.DownloadDirectory
		_ = mkdirAll(d)
		go shellOpen(d)
	case cmdSpeed:
		ns := appSettings.Update(func(s *Settings) { s.SpeedLimitOn = !s.SpeedLimitOn })
		applyRuntimeSettings(ns)
	case cmdClipboard:
		appSettings.Update(func(s *Settings) { s.WatchClipboard = !s.WatchClipboard })
	case cmdAutostart:
		_ = setAutostart(!autostartEnabled())
	case cmdQuit:
		pDestroyWindow.Call(hwnd)
	}
}

func updateTrayTooltip(active int, overall, speed float64) {
	tip := T("tipIdle")
	if active > 0 {
		s := fmtSize(int64(speed)) + "/s"
		if overall >= 0 {
			s = fmt.Sprintf("%d%% · %s", int(overall*100), s)
		}
		tip = Tf("tipActive", active, s)
	}
	tray.mu.Lock()
	if tip == tray.tip || tray.hwnd == 0 {
		tray.mu.Unlock()
		return
	}
	tray.tip = tip
	tray.mu.Unlock()
	var nid notifyIconData
	nid.init()
	nid.uFlags = nifTip
	copyU16(nid.szTip[:], tip)
	pShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

// notify shows a tray balloon (rendered as a toast on Windows 10/11).
func notify(title, body, path string) {
	tray.mu.Lock()
	if tray.hwnd == 0 {
		tray.mu.Unlock()
		return
	}
	tray.lastNotifyPath = path
	tray.mu.Unlock()
	var nid notifyIconData
	nid.init()
	nid.uFlags = nifInfo
	nid.dwInfoFlags = niifInfo
	copyU16(nid.szInfoTitle[:], title)
	copyU16(nid.szInfo[:], body)
	pShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}
