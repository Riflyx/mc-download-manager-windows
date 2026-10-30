//go:build !windows

package main

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
)

// Non-Windows stubs: lets the engine and server be built and tested on Linux/macOS.

const isWindows = false

func downloadsFolder() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads")
}
func detectLang() string                        { return "en" }
func hidePartFile(string)                       {}
func unhideFile(string)                         {}
func setSparse(*os.File)                        {}
func setLowPriority(bool)                       {}
func childProcAttr() *syscall.SysProcAttr       { return nil }
func attachKillTree(*exec.Cmd) func()           { return nil }
func shellOpen(t string)                        { log.Println("open:", t) }
func revealInExplorer(p string)                 { log.Println("reveal:", p) }
func moveToRecycleBin(p string)                 { _ = os.Remove(p) }
func browseForFolder(string) string             { return "" }
func askFileVideoIgnore(string, string) string  { return "" }
func askYesNoNever(string, string) (bool, bool) { return false, false }
func acquireSingleInstance() bool               { return true }
func autostartEnabled() bool                    { return false }
func setAutostart(bool) error                   { return nil }
func readClipboardText() string                 { return "" }
func notify(title, body, path string)           { log.Println("notify:", title, body) }
func updateTrayTooltip(int, float64, float64)   {}

func runTray(onReady func()) {
	go onReady()
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
}

func interruptProcess(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(os.Interrupt) == nil
}

func askYesNo(string, string) bool { return false }

func browserExe(string) string           { return "" }
func launchBrowser(string, string) error { return errors.New("windows only") }
