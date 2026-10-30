package main

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ToolInstaller downloads yt-dlp, ffmpeg and deno from their official GitHub releases into
// %APPDATA%\MC Download Manager\bin, verifying SHA-256 checksums.
type ToolInstaller struct {
	mu       sync.Mutex
	busy     string
	progress float64
	message  string
	verCache map[string]string
}

var tools = &ToolInstaller{verCache: map[string]string{}}

type toolSpec struct {
	assetURL string
	sumURL   string
	sumName  string            // file name inside the checksum file
	extract  map[string]string // zip entry suffix -> destination name (empty = raw exe)
	rawName  string
	size     string
}

func toolSpecs() map[string]toolSpec {
	denoTriple := "x86_64-pc-windows-msvc"
	ffArch := "win64"
	if runtime.GOARCH == "arm64" {
		denoTriple = "aarch64-pc-windows-msvc"
		ffArch = "winarm64"
	}
	ffZip := "ffmpeg-master-latest-" + ffArch + "-gpl.zip"
	denoZip := "deno-" + denoTriple + ".zip"
	return map[string]toolSpec{
		"yt-dlp": {
			assetURL: "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe",
			sumURL:   "https://github.com/yt-dlp/yt-dlp/releases/latest/download/SHA2-256SUMS",
			sumName:  "yt-dlp.exe", rawName: "yt-dlp.exe", size: "~18 MB",
		},
		"ffmpeg": {
			assetURL: "https://github.com/yt-dlp/FFmpeg-Builds/releases/download/latest/" + ffZip,
			sumURL:   "https://github.com/yt-dlp/FFmpeg-Builds/releases/download/latest/checksums.sha256",
			sumName:  ffZip,
			extract:  map[string]string{"/bin/ffmpeg.exe": "ffmpeg.exe", "/bin/ffprobe.exe": "ffprobe.exe"},
			size:     "~140 MB",
		},
		"deno": {
			assetURL: "https://github.com/denoland/deno/releases/latest/download/" + denoZip,
			sumURL:   "https://github.com/denoland/deno/releases/latest/download/" + denoZip + ".sha256sum",
			extract:  map[string]string{"deno.exe": "deno.exe"},
			size:     "~40 MB",
		},
	}
}

type ToolStatus struct {
	Busy     string            `json:"busy"`
	Progress float64           `json:"progress"`
	Message  string            `json:"message"`
	Versions map[string]string `json:"versions"`
	Paths    map[string]string `json:"paths"`
	Sizes    map[string]string `json:"sizes"`
}

func (t *ToolInstaller) Status(custom string) ToolStatus {
	paths := map[string]string{"yt-dlp": locateYtDlp(custom), "ffmpeg": locateFfmpeg(), "deno": locateDeno()}
	vers := map[string]string{}
	sizes := map[string]string{}
	for k, sp := range toolSpecs() {
		sizes[k] = sp.size
	}
	for k, p := range paths {
		if p != "" {
			vers[k] = t.version(k, p)
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return ToolStatus{Busy: t.busy, Progress: t.progress, Message: t.message, Versions: vers, Paths: paths, Sizes: sizes}
}

func (t *ToolInstaller) version(tool, p string) string {
	fi, err := os.Stat(p)
	if err != nil {
		return ""
	}
	key := p + "|" + fi.ModTime().String()
	t.mu.Lock()
	if v, ok := t.verCache[key]; ok {
		t.mu.Unlock()
		return v
	}
	t.mu.Unlock()
	arg := "--version"
	if tool == "ffmpeg" {
		arg = "-version"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p, arg)
	cmd.SysProcAttr = childProcAttr()
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if tool == "ffmpeg" {
		if f := strings.Fields(line); len(f) >= 3 {
			line = f[2]
		}
	}
	if len(line) > 60 {
		line = line[:60]
	}
	t.mu.Lock()
	t.verCache[key] = line
	t.mu.Unlock()
	return line
}

func (t *ToolInstaller) setMsg(p float64, msg string) {
	t.mu.Lock()
	t.progress, t.message = p, msg
	t.mu.Unlock()
}

// InstallAsync starts an install in the background (used by the UI).
func (t *ToolInstaller) InstallAsync(name string) error {
	if _, ok := toolSpecs()[name]; !ok {
		return errors.New("unknown tool")
	}
	t.mu.Lock()
	if t.busy != "" {
		t.mu.Unlock()
		return errors.New("another install is running")
	}
	t.mu.Unlock()
	go func() { _ = t.Install(name) }()
	return nil
}

// Install downloads, verifies and installs a tool synchronously.
func (t *ToolInstaller) Install(name string) (err error) {
	sp, ok := toolSpecs()[name]
	if !ok {
		return errors.New("unknown tool")
	}
	if !isWindows {
		return errors.New("tool install is only supported on Windows")
	}
	t.mu.Lock()
	if t.busy != "" {
		t.mu.Unlock()
		return errors.New("another install is running")
	}
	t.busy, t.progress, t.message = name, 0, name+": "+"…"
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.busy, t.progress = "", 0
		if err != nil {
			t.message = name + ": " + err.Error()
		} else {
			t.message = name + ": OK"
		}
		t.mu.Unlock()
	}()

	tmp := filepath.Join(binDir(), "."+name+".download")
	defer os.Remove(tmp)
	if err := downloadTo(sp.assetURL, tmp, func(p float64) { t.setMsg(p, fmt.Sprintf("%s: %d%%", name, int(p*100))) }); err != nil {
		return err
	}
	t.setMsg(1, name+": verifying…")
	if err := verifySHA256(tmp, sp.sumURL, sp.sumName); err != nil {
		return err
	}
	if sp.rawName != "" {
		dest := filepath.Join(binDir(), sp.rawName)
		_ = os.Remove(dest)
		if err := os.Rename(tmp, dest); err != nil {
			return err
		}
		unhideFile(dest)
		return nil
	}
	return extractZip(tmp, sp.extract)
}

func downloadTo(rawURL, dest string, onProgress func(float64)) error {
	u, _ := url.Parse(rawURL)
	done := make(chan error, 1)
	var total atomic.Int64
	total.Store(-1)
	dl := NewSegmentedDownloader(SegConfig{
		URL: u, Headers: map[string]string{"User-Agent": defaultUserAgent}, PartPath: dest, SegmentCount: 4,
		OnProbe:  func(p ProbeResult) { total.Store(p.Total) },
		OnFinish: func(err error) { done <- err },
	})
	_ = os.Remove(dest)
	dl.Start()
	tk := time.NewTicker(300 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-tk.C:
			if recv, _ := dl.Snapshot(); total.Load() > 0 {
				onProgress(float64(recv) / float64(total.Load()))
			}
		}
	}
}

func verifySHA256(file, sumURL, name string) error {
	req, _ := http.NewRequest(http.MethodGet, sumURL, nil)
	req.Header.Set("User-Agent", defaultUserAgent)
	c := &http.Client{Timeout: 30 * time.Second, Transport: httpClient.Transport}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("could not fetch checksum: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("could not fetch checksum: HTTP %d", resp.StatusCode)
	}
	expected := ""
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() && expected == "" {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if name != "" {
			// sha256sum format: "<hash>  <file>"
			if len(f) >= 2 && strings.TrimPrefix(f[len(f)-1], "*") == name && isHex64(f[0]) {
				expected = strings.ToLower(f[0])
			}
			continue
		}
		// Single-file checksum: plain "<hash>  <file>" or PowerShell Get-FileHash
		// output ("Hash : A0C3…"), as deno publishes for Windows. Take the first 64-hex token.
		for _, tok := range f {
			if isHex64(tok) {
				expected = strings.ToLower(tok)
				break
			}
		}
	}
	if len(expected) != 64 {
		return errors.New("checksum not found")
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != expected {
		return errors.New("checksum mismatch; the download was discarded")
	}
	return nil
}

func extractZip(zipPath string, want map[string]string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	found := 0
	for _, f := range zr.File {
		name := "/" + strings.ReplaceAll(f.Name, "\\", "/")
		for suffix, destName := range want {
			if !strings.HasSuffix(name, "/"+strings.TrimPrefix(suffix, "/")) {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			dest := filepath.Join(binDir(), destName)
			tmp := dest + ".new"
			out, err := os.Create(tmp)
			if err != nil {
				rc.Close()
				return err
			}
			_, cerr := io.Copy(out, rc)
			rc.Close()
			out.Close()
			if cerr != nil {
				os.Remove(tmp)
				return cerr
			}
			_ = os.Remove(dest)
			if err := os.Rename(tmp, dest); err != nil {
				return err
			}
			found++
		}
	}
	if found == 0 {
		return errors.New("archive did not contain the expected files")
	}
	return nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
