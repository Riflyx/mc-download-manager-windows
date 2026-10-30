//go:build windows && amd64

package main

import "unsafe"

// Compile-time checks that the Win32 struct layouts match the C sizes on x64.
var (
	_ [976 - unsafe.Sizeof(notifyIconData{})]byte
	_ [unsafe.Sizeof(notifyIconData{}) - 976]byte
	_ [144 - unsafe.Sizeof(jobExtendedLimit{})]byte
	_ [unsafe.Sizeof(jobExtendedLimit{}) - 144]byte
	_ [80 - unsafe.Sizeof(wndClassEx{})]byte
	_ [unsafe.Sizeof(wndClassEx{}) - 80]byte
	_ [48 - unsafe.Sizeof(winMsg{})]byte
	_ [unsafe.Sizeof(winMsg{}) - 48]byte
	_ [56 - unsafe.Sizeof(shFileOp{})]byte
	_ [unsafe.Sizeof(shFileOp{}) - 56]byte
	_ [64 - unsafe.Sizeof(browseInfo{})]byte
	_ [unsafe.Sizeof(browseInfo{}) - 64]byte
	_ [unsafe.Offsetof(notifyIconData{}.hIcon) - 32]byte
	_ [unsafe.Offsetof(notifyIconData{}.szInfo) - 304]byte
	_ [unsafe.Offsetof(notifyIconData{}.dwInfoFlags) - 948]byte
	_ [unsafe.Offsetof(shFileOp{}.fFlags) - 32]byte
)
