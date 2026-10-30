//go:build windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const isWindows = true

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	ole32    = syscall.NewLazyDLL("ole32.dll")

	pRegisterClassExW           = user32.NewProc("RegisterClassExW")
	pCreateWindowExW            = user32.NewProc("CreateWindowExW")
	pDefWindowProcW             = user32.NewProc("DefWindowProcW")
	pGetMessageW                = user32.NewProc("GetMessageW")
	pTranslateMessage           = user32.NewProc("TranslateMessage")
	pDispatchMessageW           = user32.NewProc("DispatchMessageW")
	pPostQuitMessage            = user32.NewProc("PostQuitMessage")
	pPostMessageW               = user32.NewProc("PostMessageW")
	pDestroyWindow              = user32.NewProc("DestroyWindow")
	pRegisterWindowMessageW     = user32.NewProc("RegisterWindowMessageW")
	pCreatePopupMenu            = user32.NewProc("CreatePopupMenu")
	pAppendMenuW                = user32.NewProc("AppendMenuW")
	pTrackPopupMenu             = user32.NewProc("TrackPopupMenu")
	pDestroyMenu                = user32.NewProc("DestroyMenu")
	pSetMenuDefaultItem         = user32.NewProc("SetMenuDefaultItem")
	pSetForegroundWindow        = user32.NewProc("SetForegroundWindow")
	pGetCursorPos               = user32.NewProc("GetCursorPos")
	pLoadImageW                 = user32.NewProc("LoadImageW")
	pCreateIconFromResourceEx   = user32.NewProc("CreateIconFromResourceEx")
	pMessageBoxW                = user32.NewProc("MessageBoxW")
	pAddClipboardFormatListener = user32.NewProc("AddClipboardFormatListener")
	pOpenClipboard              = user32.NewProc("OpenClipboard")
	pCloseClipboard             = user32.NewProc("CloseClipboard")
	pGetClipboardData           = user32.NewProc("GetClipboardData")
	pIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")
	pGetSystemMetrics           = user32.NewProc("GetSystemMetrics")

	pShellNotifyIconW      = shell32.NewProc("Shell_NotifyIconW")
	pShellExecuteW         = shell32.NewProc("ShellExecuteW")
	pSHQueryUserNotifState = shell32.NewProc("SHQueryUserNotificationState")
	pSHFileOperationW      = shell32.NewProc("SHFileOperationW")
	pSHBrowseForFolderW    = shell32.NewProc("SHBrowseForFolderW")
	pSHGetPathFromIDListW  = shell32.NewProc("SHGetPathFromIDListW")
	pSHGetKnownFolderPath  = shell32.NewProc("SHGetKnownFolderPath")
	pCoInitializeEx        = ole32.NewProc("CoInitializeEx")
	pCoTaskMemFree         = ole32.NewProc("CoTaskMemFree")
	pCreateMutexW          = kernel32.NewProc("CreateMutexW")
	pGlobalLock            = kernel32.NewProc("GlobalLock")
	pGlobalUnlock          = kernel32.NewProc("GlobalUnlock")
	pGlobalSize            = kernel32.NewProc("GlobalSize")
	pGetModuleHandleW      = kernel32.NewProc("GetModuleHandleW")
	pSetPriorityClass      = kernel32.NewProc("SetPriorityClass")
	pGetCurrentProcess     = kernel32.NewProc("GetCurrentProcess")
	pDeviceIoControl       = kernel32.NewProc("DeviceIoControl")
	pCreateJobObjectW      = kernel32.NewProc("CreateJobObjectW")
	pSetInformationJobObj  = kernel32.NewProc("SetInformationJobObject")
	pAssignProcessToJobObj = kernel32.NewProc("AssignProcessToJobObject")
	pTerminateJobObject    = kernel32.NewProc("TerminateJobObject")
	pOpenProcess           = kernel32.NewProc("OpenProcess")
	pGetUserDefaultUILang  = kernel32.NewProc("GetUserDefaultUILanguage")
	pSetFileAttributesW    = kernel32.NewProc("SetFileAttributesW")
	pGetFileAttributesW    = kernel32.NewProc("GetFileAttributesW")
	pRegOpenKeyExW         = advapi32.NewProc("RegOpenKeyExW")
	pRegSetValueExW        = advapi32.NewProc("RegSetValueExW")
	pRegDeleteValueW       = advapi32.NewProc("RegDeleteValueW")
	pRegQueryValueExW      = advapi32.NewProc("RegQueryValueExW")
	pRegCloseKey           = advapi32.NewProc("RegCloseKey")
)

func u16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(strings.ReplaceAll(s, "\x00", ""))
	return p
}

func copyU16(dst []uint16, s string) {
	src := syscall.StringToUTF16(s)
	if len(src) > len(dst) {
		src = src[:len(dst)]
		src[len(src)-1] = 0
	}
	copy(dst, src)
}

// ---------------------------------------------------------------------------------------------
// Misc platform helpers

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var folderIDDownloads = guid{0x374DE290, 0x123F, 0x4565, [8]byte{0x91, 0x64, 0x39, 0xC4, 0x92, 0x5E, 0x46, 0x7B}}

func downloadsFolder() string {
	var p *uint16
	r, _, _ := pSHGetKnownFolderPath.Call(uintptr(unsafe.Pointer(&folderIDDownloads)), 0, 0, uintptr(unsafe.Pointer(&p)))
	if r == 0 && p != nil {
		s := utf16PtrToString(p)
		pCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
		if s != "" {
			return s
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads")
}

func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	var buf []uint16
	for ptr := unsafe.Pointer(p); ; ptr = unsafe.Add(ptr, 2) {
		c := *(*uint16)(ptr)
		if c == 0 {
			break
		}
		buf = append(buf, c)
		if len(buf) > 1<<20 {
			break
		}
	}
	return syscall.UTF16ToString(buf)
}

func detectLang() string {
	r, _, _ := pGetUserDefaultUILang.Call()
	if r&0x3FF == 0x1F {
		return "tr"
	}
	return "en"
}

const (
	fileAttrHidden  = 0x2
	invalidFileAttr = 0xFFFFFFFF
)

func hidePartFile(p string) {
	a, _, _ := pGetFileAttributesW.Call(uintptr(unsafe.Pointer(u16(p))))
	if a != invalidFileAttr {
		pSetFileAttributesW.Call(uintptr(unsafe.Pointer(u16(p))), a|fileAttrHidden)
	}
}

func unhideFile(p string) {
	a, _, _ := pGetFileAttributesW.Call(uintptr(unsafe.Pointer(u16(p))))
	if a != invalidFileAttr && a&fileAttrHidden != 0 {
		pSetFileAttributesW.Call(uintptr(unsafe.Pointer(u16(p))), a&^fileAttrHidden)
	}
}

func setSparse(f *os.File) {
	const fsctlSetSparse = 0x000900C4
	var ret uint32
	pDeviceIoControl.Call(f.Fd(), fsctlSetSparse, 0, 0, 0, 0, uintptr(unsafe.Pointer(&ret)), 0)
}

const (
	belowNormalPriorityClass = 0x00004000
	normalPriorityClass      = 0x00000020
	processModeBgBegin       = 0x00100000
	processModeBgEnd         = 0x00200000
	createNoWindow           = 0x08000000
)

var lowPrio struct {
	sync.Mutex
	on, set bool
}

// setLowPriority runs the app (and its yt-dlp children) at below-normal CPU priority with
// background I/O priority, so downloads never compete with games or foreground work.
func setLowPriority(on bool) {
	lowPrio.Lock()
	defer lowPrio.Unlock()
	if lowPrio.set && lowPrio.on == on {
		return
	}
	h, _, _ := pGetCurrentProcess.Call()
	if on {
		pSetPriorityClass.Call(h, processModeBgBegin)
		pSetPriorityClass.Call(h, belowNormalPriorityClass)
	} else {
		if lowPrio.set {
			pSetPriorityClass.Call(h, processModeBgEnd)
		}
		pSetPriorityClass.Call(h, normalPriorityClass)
	}
	lowPrio.on, lowPrio.set = on, true
}

func childProcAttr() *syscall.SysProcAttr {
	flags := uint32(createNoWindow)
	if appSettings == nil || appSettings.Get().LowPriority {
		flags |= belowNormalPriorityClass
	}
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
}

type jobBasicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobExtendedLimit struct {
	Basic                 jobBasicLimit
	IoInfo                [6]uint64
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// attachKillTree puts the child in a job object so pausing kills yt-dlp *and* the Python
// child the onefile exe spawns, and nothing survives if the app exits.
func attachKillTree(cmd *exec.Cmd) func() {
	job, _, _ := pCreateJobObjectW.Call(0, 0)
	if job == 0 || cmd.Process == nil {
		return nil
	}
	var info jobExtendedLimit
	info.Basic.LimitFlags = 0x2000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	pSetInformationJobObj.Call(job, 9, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	ph, _, _ := pOpenProcess.Call(0x0100|0x0001, 0, uintptr(cmd.Process.Pid)) // SET_QUOTA | TERMINATE
	if ph != 0 {
		pAssignProcessToJobObj.Call(job, ph)
		syscall.CloseHandle(syscall.Handle(ph))
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			pTerminateJobObject.Call(job, 1)
			syscall.CloseHandle(syscall.Handle(job))
		})
	}
}

func shellOpen(target string) {
	pShellExecuteW.Call(0, uintptr(unsafe.Pointer(u16("open"))), uintptr(unsafe.Pointer(u16(target))), 0, 0, 1)
}

func revealInExplorer(p string) {
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + strings.ReplaceAll(p, `"`, "") + `"`}
	_ = cmd.Start()
	go func() { _ = cmd.Wait() }()
}

type shFileOp struct {
	hwnd                  uintptr
	wFunc                 uint32
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

func moveToRecycleBin(p string) {
	from := append(syscall.StringToUTF16(p), 0) // double-null terminated
	op := shFileOp{wFunc: 3, pFrom: &from[0], fFlags: 0x40 | 0x10 | 0x4 | 0x400}
	r, _, _ := pSHFileOperationW.Call(uintptr(unsafe.Pointer(&op)))
	if r != 0 {
		_ = os.Remove(p)
	}
}

type browseInfo struct {
	hwndOwner      uintptr
	pidlRoot       uintptr
	pszDisplayName *uint16
	lpszTitle      *uint16
	ulFlags        uint32
	lpfn           uintptr
	lParam         uintptr
	iImage         int32
}

func browseForFolder(current string) string {
	res := make(chan string, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		pCoInitializeEx.Call(0, 2)
		name := make([]uint16, 260)
		bi := browseInfo{pszDisplayName: &name[0], lpszTitle: u16(appName), ulFlags: 0x1 | 0x40}
		pidl, _, _ := pSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
		if pidl == 0 {
			res <- ""
			return
		}
		buf := make([]uint16, 1024)
		pSHGetPathFromIDListW.Call(pidl, uintptr(unsafe.Pointer(&buf[0])))
		pCoTaskMemFree.Call(pidl)
		res <- syscall.UTF16ToString(buf)
	}()
	return <-res
}

const (
	mbYesNoCancel = 0x3
	mbYesNo       = 0x4
	mbIconQ       = 0x20
	mbIconInfo    = 0x40
	mbTopMost     = 0x40000
	mbSetFg       = 0x10000
	idYes         = 6
	idNo          = 7
)

func msgBox(title, text string, flags uintptr) int {
	r, _, _ := pMessageBoxW.Call(0, uintptr(unsafe.Pointer(u16(text))), uintptr(unsafe.Pointer(u16(title))), flags|mbTopMost|mbSetFg)
	return int(r)
}

func askFileVideoIgnore(title, text string) string {
	switch msgBox(title, text, mbYesNoCancel|mbIconQ) {
	case idYes:
		return "file"
	case idNo:
		return "video"
	}
	return ""
}

// askYesNoNever: Yes = install, No = not now, Cancel = don't ask again.
func askYesNoNever(title, text string) (yes, never bool) {
	switch msgBox(title, title+"\n\n"+text, mbYesNoCancel|mbIconQ) {
	case idYes:
		return true, false
	case idNo:
		return false, false
	}
	return false, true
}

// ---------------------------------------------------------------------------------------------
// Single instance & autostart

var instanceMutex uintptr

func acquireSingleInstance() bool {
	h, _, err := pCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(u16(`Local\MCDownloadManager.Win`))))
	if h == 0 {
		return true
	}
	if errno, ok := err.(syscall.Errno); ok && errno == 183 { // ERROR_ALREADY_EXISTS
		return false
	}
	instanceMutex = h
	return true
}

const (
	hkcu     = 0x80000001
	runKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue = "MC Download Manager"
	keyQuery = 0x0001
	keySet   = 0x0002
	regSZ    = 1
)

func autostartCommand() string {
	exe, _ := os.Executable()
	return `"` + exe + `" --background`
}

func openRunKey(access uintptr) (uintptr, bool) {
	var h uintptr
	r, _, _ := pRegOpenKeyExW.Call(hkcu, uintptr(unsafe.Pointer(u16(runKey))), 0, access, uintptr(unsafe.Pointer(&h)))
	return h, r == 0
}

func readAutostartValue() (string, bool) {
	h, ok := openRunKey(keyQuery)
	if !ok {
		return "", false
	}
	defer pRegCloseKey.Call(h)
	var typ, size uint32
	r, _, _ := pRegQueryValueExW.Call(h, uintptr(unsafe.Pointer(u16(runValue))), 0, uintptr(unsafe.Pointer(&typ)), 0, uintptr(unsafe.Pointer(&size)))
	if r != 0 || size == 0 {
		return "", false
	}
	buf := make([]uint16, size/2+1)
	r, _, _ = pRegQueryValueExW.Call(h, uintptr(unsafe.Pointer(u16(runValue))), 0, uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r != 0 {
		return "", false
	}
	return syscall.UTF16ToString(buf), true
}

func autostartEnabled() bool {
	_, ok := readAutostartValue()
	return ok
}

func setAutostart(on bool) error {
	h, ok := openRunKey(keySet | keyQuery)
	if !ok {
		return syscall.EINVAL
	}
	defer pRegCloseKey.Call(h)
	if !on {
		pRegDeleteValueW.Call(h, uintptr(unsafe.Pointer(u16(runValue))))
		return nil
	}
	data := syscall.StringToUTF16(autostartCommand())
	r, _, _ := pRegSetValueExW.Call(h, uintptr(unsafe.Pointer(u16(runValue))), 0, regSZ,
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)*2))
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

// refreshAutostartPath keeps the Run entry pointing at the current exe if the user moved it.
func refreshAutostartPath() {
	if v, ok := readAutostartValue(); ok && v != autostartCommand() {
		_ = setAutostart(true)
	}
}

// ---------------------------------------------------------------------------------------------
// Clipboard

func readClipboardText() string {
	const cfUnicodeText = 13
	if r, _, _ := pIsClipboardFormatAvailable.Call(cfUnicodeText); r == 0 {
		return ""
	}
	tray.mu.Lock()
	hwnd := tray.hwnd
	tray.mu.Unlock()
	ok := false
	for i := 0; i < 5; i++ {
		if r, _, _ := pOpenClipboard.Call(hwnd); r != 0 {
			ok = true
			break
		}
	}
	if !ok {
		return ""
	}
	defer pCloseClipboard.Call()
	h, _, _ := pGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return ""
	}
	size, _, _ := pGlobalSize.Call(h)
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		return ""
	}
	defer pGlobalUnlock.Call(h)
	n := int(size / 2)
	if n > 64<<10 {
		n = 64 << 10
	}
	s := unsafe.Slice((*uint16)(unsafe.Pointer(p)), n)
	for i, c := range s {
		if c == 0 {
			s = s[:i]
			break
		}
	}
	return syscall.UTF16ToString(s)
}

// ---------------------------------------------------------------------------------------------
// Graceful stop for console children (yt-dlp): deliver a Ctrl+C event through their console.

var (
	pAttachConsole            = kernel32.NewProc("AttachConsole")
	pFreeConsole              = kernel32.NewProc("FreeConsole")
	pGenerateConsoleCtrlEvent = kernel32.NewProc("GenerateConsoleCtrlEvent")
	ctrlMu                    sync.Mutex
	ctrlOnce                  sync.Once
)

func interruptProcess(pid int) bool {
	ctrlOnce.Do(func() {
		// While attached to the child's console we receive the same Ctrl+C. Registering for
		// os.Interrupt makes Go's console handler report it as handled instead of exiting.
		// (SetConsoleCtrlHandler(NULL, TRUE) is avoided on purpose: it is inherited by children
		// started later, which would then ignore Ctrl+C.)
		c := make(chan os.Signal, 4)
		signal.Notify(c, os.Interrupt)
		go func() {
			for range c {
			}
		}()
	})
	ctrlMu.Lock()
	defer ctrlMu.Unlock()
	pFreeConsole.Call()
	if r, _, _ := pAttachConsole.Call(uintptr(pid)); r == 0 {
		return false
	}
	r, _, _ := pGenerateConsoleCtrlEvent.Call(0, 0) // CTRL_C_EVENT to every process on that console
	time.Sleep(200 * time.Millisecond)              // let the event reach our own handler before detaching
	pFreeConsole.Call()
	return r != 0
}

func askYesNo(title, text string) bool { return msgBox(title, text, mbYesNo|mbIconQ) == idYes }

// browserExe returns the install path of "chrome", "edge" or "brave", or "" when not installed.
func browserExe(name string) string {
	rel := map[string]string{
		"chrome": `Google\Chrome\Application\chrome.exe`,
		"edge":   `Microsoft\Edge\Application\msedge.exe`,
		"brave":  `BraveSoftware\Brave-Browser\Application\brave.exe`,
	}[name]
	if rel == "" {
		return ""
	}
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
		if base := os.Getenv(env); base != "" {
			if p := filepath.Join(base, rel); fileExists(p) {
				return p
			}
		}
	}
	return ""
}

// launchBrowser opens url in exe at normal priority (our own below-normal class would otherwise be inherited).
func launchBrowser(exe, url string) error {
	cmd := exec.Command(exe, url)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: normalPriorityClass}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// isFullscreenApp reports whether a fullscreen app (game, presentation) owns the screen: the shell's
// QUNS_BUSY, QUNS_RUNNING_D3D_FULL_SCREEN and QUNS_PRESENTATION_MODE states.
func isFullscreenApp() bool {
	var state uint32
	if r, _, _ := pSHQueryUserNotifState.Call(uintptr(unsafe.Pointer(&state))); r != 0 {
		return false
	}
	return state == 2 || state == 3 || state == 4
}
