package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
)

//go:embed all:extension
var extensionFS embed.FS

var (
	appSettings *SettingsStore
	appStore    *Store
	appServer   *Server
)

func main() {
	background := false
	for _, a := range os.Args[1:] {
		if a == "--background" || a == "/background" {
			background = true
		}
	}
	lang = detectLang()

	// Keep the resident footprint small: collect more eagerly and cap the heap softly.
	debug.SetGCPercent(50)
	debug.SetMemoryLimit(64 << 20)

	firstRun := !fileExists(filepath.Join(supportDir(), "settings.json"))
	appSettings = loadSettings()

	if !acquireSingleInstance() {
		// Already running: just bring up its UI.
		st := appSettings.Get()
		shellOpen(fmt.Sprintf("http://127.0.0.1:%d/ui/%s/", st.ServerPort, st.Token))
		return
	}

	applyRuntimeSettings(appSettings.Get())
	appStore = NewStore(appSettings)
	appServer = NewServer(appStore, appSettings)
	scheduleYtdlpAutoUpdate()
	scheduleUpdateCheck()
	serverErr := appServer.Start()
	ensureExtensionFiles()

	appStore.OnActivity = func(active int, overall, speed float64) { updateTrayTooltip(active, overall, speed) }

	runTray(func() {
		if serverErr != nil {
			notify(appName, Tf("portBusy", appSettings.Get().ServerPort), "")
		}
		if firstRun || !background {
			openUI()
		}
	})
	appStore.Shutdown()
}

func openUI() { shellOpen(appServer.UIURL()) }

// applyRuntimeSettings pushes settings that act on the running process.
func applyRuntimeSettings(s Settings) {
	applyRate(s)
	setLowPriority(s.LowPriority)
}

func onServerRestarted() {}

func mkdirAll(d string) error { return os.MkdirAll(d, 0o755) }

// ---------------------------------------------------------------------------------------------
// Bundled browser extension (loaded via chrome://extensions → Load unpacked)

func extensionDir() string { return filepath.Join(supportDir(), "extension") }

var extOnce sync.Once

func ensureExtensionFiles() string {
	dir := extensionDir()
	extOnce.Do(func() {
		// Always refresh files whose content differs (a stale stamp once left the browser on an old extension).
		_ = fs.WalkDir(extensionFS, "extension", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel := strings.TrimPrefix(strings.TrimPrefix(p, "extension"), "/")
			dst := filepath.Join(dir, filepath.FromSlash(rel))
			if d.IsDir() {
				return os.MkdirAll(dst, 0o755)
			}
			b, err := extensionFS.ReadFile(p)
			if err == nil {
				if old, e := os.ReadFile(dst); e != nil || !bytes.Equal(old, b) {
					_ = os.WriteFile(dst, b, 0o644)
				}
			}
			return nil
		})
	})
	return dir
}

// ---------------------------------------------------------------------------------------------
// Clipboard watching (event-driven on Windows: WM_CLIPBOARDUPDATE, no polling)

var (
	clipPrompting atomic.Bool
	clipMu        sync.Mutex
	clipAsked     []string
)

var videoHosts = []string{"youtube.com", "youtu.be", "vimeo.com", "dailymotion.com", "twitter.com", "x.com",
	"instagram.com", "tiktok.com", "twitch.tv", "facebook.com", "reddit.com", "bilibili.com", "kick.com"}

func urlOnlyContent(text string) []*url.URL {
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 || len(lines) > 50 {
		return nil
	}
	var out []*url.URL
	seen := map[string]bool{}
	for _, l := range lines {
		if strings.ContainsAny(l, " \t") {
			return nil
		}
		u, err := url.Parse(l)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil
		}
		if !seen[u.String()] {
			seen[u.String()] = true
			out = append(out, u)
		}
	}
	return out
}

func looksLikeVideoPage(u *url.URL) bool {
	if categoryOf(filepath.Base(u.Path), "file") != "other" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, v := range videoHosts {
		if h == v || strings.HasSuffix(h, "."+v) {
			return true
		}
	}
	return false
}

func onClipboardChanged() {
	st := appSettings.Get()
	if !st.WatchClipboard || clipPrompting.Load() {
		return
	}
	text := readClipboardText()
	if text == "" || len(text) > 64<<10 {
		return
	}
	all := urlOnlyContent(text)
	if all == nil {
		return
	}
	exts := st.CaptureExtensions()
	var urls, videos []*url.URL
	clipMu.Lock()
	for _, u := range all {
		if st.ClipboardVideos && looksLikeVideoPost(u) && !contains(clipAsked, u.String()) {
			videos = append(videos, u)
			clipAsked = append(clipAsked, u.String())
			continue
		}
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(u.Path), "."))
		if ext == "" || !contains(exts, ext) || contains(clipAsked, u.String()) {
			continue
		}
		urls = append(urls, u)
		clipAsked = append(clipAsked, u.String())
	}
	if len(clipAsked) > 50 {
		clipAsked = clipAsked[len(clipAsked)-50:]
	}
	clipMu.Unlock()
	if len(urls) == 0 && len(videos) > 0 {
		clipPrompting.Store(true)
		go func() {
			defer clipPrompting.Store(false)
			if askYesNo(T("clipTitle"), Tf("clipVideo", videos[0].String())) {
				for _, u := range videos {
					appStore.Add(NewDownloadRequest{URL: u, Kind: "video"})
				}
			}
		}()
		return
	}
	if len(urls) == 0 {
		return
	}
	clipPrompting.Store(true)
	go func() {
		defer clipPrompting.Store(false)
		var msg string
		if len(urls) == 1 {
			msg = Tf("clipOne", urls[0].String())
		} else {
			var ls []string
			for i, u := range urls {
				if i == 6 {
					ls = append(ls, "…")
					break
				}
				ls = append(ls, u.String())
			}
			msg = Tf("clipMany", len(urls), strings.Join(ls, "\n"))
		}
		kind := askFileVideoIgnore(T("clipTitle"), msg)
		if kind == "" {
			return
		}
		for _, u := range urls {
			appStore.Add(NewDownloadRequest{URL: u, Kind: kind})
		}
	}()
}

// expectedExtVersion is the version of the extension bundled in this exe (what the browser should be running).
func expectedExtVersion() string {
	b, err := extensionFS.ReadFile("extension/manifest.json")
	if err != nil {
		return ""
	}
	var m struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(b, &m)
	return m.Version
}
