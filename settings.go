package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const appVersion = "1.2.6-win"
const appName = "MC Download Manager"

// Settings is persisted as JSON in %APPDATA%\MC Download Manager\settings.json.
type Settings struct {
	SegmentCount       int      `json:"segmentCount"`
	MaxConcurrent      int      `json:"maxConcurrent"`
	ServerPort         int      `json:"serverPort"`
	DownloadDirectory  string   `json:"downloadDirectory"`
	YtdlpPath          string   `json:"ytdlpPath"`
	VideoFormat        string   `json:"videoFormat"`
	VideoContainer     string   `json:"videoContainer"` // mp4 | mkv | webm | original
	VideoMaxHeight     int      `json:"videoMaxHeight"` // 0 best, -1 m4a, -2 mp3
	AutoStart          bool     `json:"autoStart"`      // start queued downloads automatically
	NotifyOnComplete   bool     `json:"notifyOnComplete"`
	WatchClipboard     bool     `json:"watchClipboard"`
	CaptureGroupIDs    []string `json:"captureGroupIDs"`
	CustomExtensions   string   `json:"customExtensions"`
	OrganizeByCategory bool     `json:"organizeByCategory"`
	SpeedLimitKBps     int      `json:"speedLimitKBps"` // 0 = no limit configured
	SpeedLimitOn       bool     `json:"speedLimitOn"`
	LowPriority        bool     `json:"lowPriority"` // below-normal CPU + background I/O priority
	DenoPromptOff      bool     `json:"denoPromptOff"`
	AutostartOffered   bool     `json:"autostartOffered"`
	AutoUpdateYtdlp    bool     `json:"autoUpdateYtdlp"`
	LastYtdlpUpdate    int64    `json:"lastYtdlpUpdate"`
	ClipboardVideos    bool     `json:"clipboardVideos"` // also ask for copied social-media video links
	LiveFromStart      bool     `json:"liveFromStart"`   // record YouTube/Twitch live streams from the beginning // Run-key entry was added once on first launch
	Token              string   `json:"token"`           // secret for the local web UI
}

var captureGroups = []struct {
	ID   string
	Exts []string
}{
	{"compressed", []string{"zip", "rar", "7z", "tar", "gz", "tgz", "bz2", "xz", "zst", "iso", "img", "cab"}},
	{"apps", []string{"exe", "msi", "msix", "appx", "apk", "dmg", "pkg", "deb", "rpm", "appimage", "jar", "bin"}},
	{"video", []string{"mp4", "m4v", "mkv", "webm", "mov", "avi", "flv", "wmv", "mpg", "mpeg", "m3u8", "3gp"}},
	{"music", []string{"mp3", "m4a", "aac", "flac", "wav", "ogg", "opus", "wma", "aiff"}},
	{"document", []string{"pdf", "epub", "mobi", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "odt", "ods", "odp", "rtf", "csv"}},
	{"image", []string{"jpg", "jpeg", "png", "gif", "webp", "heic", "bmp", "tiff", "psd", "svg", "raw"}},
}

func defaultSettings() Settings {
	return Settings{
		SegmentCount:       8,
		MaxConcurrent:      3,
		ServerPort:         48213,
		DownloadDirectory:  filepath.Join(downloadsFolder(), appName),
		VideoContainer:     "mp4",
		VideoMaxHeight:     1080,
		AutoStart:          true,
		NotifyOnComplete:   true,
		WatchClipboard:     true,
		CaptureGroupIDs:    []string{"compressed", "apps", "video", "music", "document"},
		OrganizeByCategory: true,
		SpeedLimitKBps:     0,
		LowPriority:        true,
		LiveFromStart:      true,
		AutoUpdateYtdlp:    true,
	}
}

type SettingsStore struct {
	mu   sync.RWMutex
	s    Settings
	path string
}

func supportDir() string {
	base := os.Getenv("APPDATA")
	if base == "" {
		base, _ = os.UserConfigDir()
	}
	d := filepath.Join(base, appName)
	_ = os.MkdirAll(d, 0o700)
	return d
}

func binDir() string {
	d := filepath.Join(supportDir(), "bin")
	_ = os.MkdirAll(d, 0o755)
	return d
}

func loadSettings() *SettingsStore {
	st := &SettingsStore{s: defaultSettings(), path: filepath.Join(supportDir(), "settings.json")}
	if b, err := os.ReadFile(st.path); err == nil {
		_ = json.Unmarshal(b, &st.s)
	}
	st.s.normalize()
	if st.s.Token == "" {
		buf := make([]byte, 16)
		_, _ = rand.Read(buf)
		st.s.Token = hex.EncodeToString(buf)
	}
	st.save()
	return st
}

func (s *Settings) normalize() {
	s.SegmentCount = clamp(s.SegmentCount, 1, 32)
	s.MaxConcurrent = clamp(s.MaxConcurrent, 1, 10)
	s.ServerPort = clamp(s.ServerPort, 1024, 65535)
	if s.SpeedLimitKBps < 0 {
		s.SpeedLimitKBps = 0
	}
	switch s.VideoContainer {
	case "mp4", "mkv", "webm", "original":
	default:
		s.VideoContainer = "mp4"
	}
	if strings.TrimSpace(s.DownloadDirectory) == "" {
		s.DownloadDirectory = defaultSettings().DownloadDirectory
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (st *SettingsStore) Get() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	c := st.s
	c.CaptureGroupIDs = append([]string(nil), st.s.CaptureGroupIDs...)
	return c
}

// Update applies fn to a copy, normalizes and persists it.
func (st *SettingsStore) Update(fn func(*Settings)) Settings {
	st.mu.Lock()
	token := st.s.Token
	fn(&st.s)
	st.s.Token = token // never changed through updates
	st.s.normalize()
	st.mu.Unlock()
	st.save()
	return st.Get()
}

func (st *SettingsStore) save() {
	st.mu.RLock()
	b, err := json.MarshalIndent(st.s, "", "  ")
	st.mu.RUnlock()
	if err != nil {
		return
	}
	writeFileAtomic(st.path, b)
}

func writeFileAtomic(path string, data []byte) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

func (s Settings) CaptureExtensions() []string {
	set := map[string]bool{}
	var out []string
	for _, g := range captureGroups {
		if contains(s.CaptureGroupIDs, g.ID) {
			for _, e := range g.Exts {
				if !set[e] {
					set[e] = true
					out = append(out, e)
				}
			}
		}
	}
	for _, tok := range strings.FieldsFunc(strings.ToLower(s.CustomExtensions), func(r rune) bool {
		return r == ',' || r == ' ' || r == ';' || r == '\n' || r == '\r' || r == '\t'
	}) {
		e := strings.Trim(tok, ". ")
		if e != "" && !set[e] {
			set[e] = true
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// SpeedLimitBytes returns the active global limit in bytes/s (0 = unlimited).
func (s Settings) SpeedLimitBytes() int64 {
	if !s.SpeedLimitOn || s.SpeedLimitKBps <= 0 {
		return 0
	}
	return int64(s.SpeedLimitKBps) * 1024
}

// YtdlpFormatArgs mirrors the macOS app's format selection. Without ffmpeg, yt-dlp cannot merge
// separate video/audio streams or extract audio, so single-file formats are requested instead.
func (s Settings) YtdlpFormatArgs(haveFfmpeg bool) []string {
	custom := strings.TrimSpace(s.VideoFormat)
	merge := s.VideoContainer
	if merge == "original" {
		merge = "mkv"
	}
	if custom != "" {
		if haveFfmpeg {
			return []string{"-f", custom, "--merge-output-format", merge}
		}
		return []string{"-f", custom}
	}
	if s.VideoMaxHeight == -1 || s.VideoMaxHeight == -2 {
		if !haveFfmpeg {
			return []string{"-f", "ba[ext=m4a]/ba/b"}
		}
		f := "m4a"
		if s.VideoMaxHeight == -2 {
			f = "mp3"
		}
		return []string{"-f", "ba/b", "-x", "--audio-format", f, "--audio-quality", "0"}
	}
	h := ""
	if s.VideoMaxHeight > 0 {
		h = "[height<=" + itoa(s.VideoMaxHeight) + "]"
	}
	if !haveFfmpeg {
		switch s.VideoContainer {
		case "mp4":
			return []string{"-f", "b" + h + "[ext=mp4]/b" + h + "/b"}
		case "webm":
			return []string{"-f", "b" + h + "[ext=webm]/b" + h + "/b"}
		}
		return []string{"-f", "b" + h + "/b"}
	}
	switch s.VideoContainer {
	case "mp4":
		return []string{"-f", "bv*" + h + "[vcodec^=avc1]+ba[acodec^=mp4a]/bv*" + h + "[ext=mp4]+ba[ext=m4a]/bv*" + h + "+ba/b" + h + "/b",
			"--merge-output-format", "mp4", "--remux-video", "mp4"}
	case "webm":
		return []string{"-f", "bv*" + h + "[ext=webm]+ba[ext=webm]/bv*" + h + "+ba/b" + h + "/b", "--merge-output-format", "webm"}
	case "mkv":
		return []string{"-f", "bv*" + h + "+ba/b" + h + "/b", "--merge-output-format", "mkv"}
	}
	return []string{"-f", "bv*" + h + "+ba/b" + h + "/b"}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
