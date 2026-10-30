package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	StQueued      = "queued"
	StProbing     = "probing"
	StDownloading = "downloading"
	StPaused      = "paused"
	StCompleted   = "completed"
	StFailed      = "failed"
	StCancelled   = "cancelled"
)

func isActiveState(s string) bool { return s == StProbing || s == StDownloading }

type DownloadItem struct {
	ID                string            `json:"id"`
	URL               string            `json:"url"`
	FileName          string            `json:"fileName"`
	Directory         string            `json:"directory"`
	Kind              string            `json:"kind"` // file | video
	State             string            `json:"state"`
	TotalBytes        int64             `json:"totalBytes"`
	ReceivedBytes     int64             `json:"receivedBytes"`
	SupportsRanges    bool              `json:"supportsRanges"`
	Segments          []Segment         `json:"segments,omitempty"`
	Headers           map[string]string `json:"headers,omitempty"`
	CookieFile        string            `json:"cookieFile,omitempty"`
	ErrorMessage      string            `json:"errorMessage,omitempty"`
	CreatedAt         time.Time         `json:"createdAt"`
	CompletedAt       *time.Time        `json:"completedAt,omitempty"`
	FinalPath         string            `json:"finalPath,omitempty"`
	RequestedSegments int               `json:"requestedSegments,omitempty"`
	Section           string            `json:"section,omitempty"`
	SourceURL         string            `json:"sourceUrl,omitempty"` // URL before redirects (dedupe key)

	Speed    float64   `json:"-"`
	Log      string    `json:"-"`
	Stage    string    `json:"-"`
	Live     bool      `json:"-"`
	Stopping bool      `json:"-"`
	Started  time.Time `json:"-"`
	LastDest string    `json:"-"` // last yt-dlp "Destination:" path (for rescuing a killed live recording)
}

func (it *DownloadItem) partPath() string {
	return filepath.Join(it.Directory, ".dm-"+it.ID+".part")
}

func (it *DownloadItem) progress() float64 {
	if it.TotalBytes <= 0 {
		return 0
	}
	p := float64(it.ReceivedBytes) / float64(it.TotalBytes)
	if p > 1 {
		p = 1
	}
	return p
}

type NewDownloadRequest struct {
	URL          *url.URL
	FileName     string
	Referer      string
	UserAgent    string
	CookieHeader string
	Cookies      []map[string]any
	Kind         string
	ExtraHeaders map[string]string
	Segments     int
	Section      string
	SourceURL    string // pre-redirect URL from the browser, used to detect repeated clicks
}

type videoPhase struct{ base, last, approx int64 }
type speedSample struct {
	bytes int64
	t     time.Time
}

type Store struct {
	mu          sync.Mutex
	items       []*DownloadItem
	settings    *SettingsStore
	downloaders map[string]*SegmentedDownloader
	videos      map[string]*VideoJob
	phases      map[string]*videoPhase
	samples     map[string]speedSample
	cookieless  map[string]bool

	denoWaiters   []string
	denoPrompting bool
	denoDeclined  bool

	tickerOn  bool
	tickCount int
	saveTimer *time.Timer
	storePath string

	// OnActivity is called (without the lock held) when the active set changes and on every tick
	// while downloads run. Used for the tray tooltip.
	OnActivity func(active int, overall float64, speed float64)
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := strings.ToUpper(hex.EncodeToString(b))
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func NewStore(settings *SettingsStore) *Store {
	s := &Store{
		settings:    settings,
		downloaders: map[string]*SegmentedDownloader{},
		videos:      map[string]*VideoJob{},
		phases:      map[string]*videoPhase{},
		samples:     map[string]speedSample{},
		cookieless:  map[string]bool{},
		storePath:   filepath.Join(supportDir(), "downloads.json"),
	}
	s.load()
	return s
}

// ---------------------------------------------------------------------------------------------
// Queries

func (s *Store) index(id string) int {
	for i, it := range s.items {
		if it.ID == id {
			return i
		}
	}
	return -1
}

func (s *Store) get(id string) *DownloadItem {
	if i := s.index(id); i >= 0 {
		return s.items[i]
	}
	return nil
}

func (s *Store) activeCountLocked() int {
	n := 0
	for _, it := range s.items {
		if isActiveState(it.State) {
			n++
		}
	}
	return n
}

func (s *Store) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeCountLocked()
}

func categoryOf(fileName, kind string) string {
	if kind == "video" {
		return "video"
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fileName), "."))
	if ext == "" {
		return "other"
	}
	for cat, exts := range categoryTable {
		for _, e := range exts {
			if e == ext {
				return cat
			}
		}
	}
	return "other"
}

var categoryTable = map[string][]string{
	"image":      {"jpg", "jpeg", "png", "gif", "webp", "heic", "heif", "bmp", "tiff", "tif", "svg", "psd", "ai", "raw", "cr2", "nef", "ico"},
	"music":      {"mp3", "m4a", "aac", "flac", "wav", "ogg", "opus", "wma", "aiff", "alac"},
	"video":      {"mp4", "m4v", "mkv", "webm", "mov", "avi", "flv", "wmv", "mpg", "mpeg", "ts", "m3u8", "3gp"},
	"apps":       {"exe", "msi", "msix", "appx", "apk", "dmg", "pkg", "ipa", "deb", "rpm", "appimage", "jar", "xpi", "crx"},
	"document":   {"pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "rtf", "odt", "ods", "odp", "epub", "mobi", "csv", "md"},
	"compressed": {"zip", "rar", "7z", "tar", "gz", "tgz", "bz2", "xz", "zst", "iso", "img", "cab"},
}

func (s *Store) destDir(it *DownloadItem) string {
	if !s.settings.Get().OrganizeByCategory {
		return it.Directory
	}
	return filepath.Join(it.Directory, T("cat"+categoryOf(it.FileName, it.Kind)))
}

// ---------------------------------------------------------------------------------------------
// Adding

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36"

// Add is used for explicit adds (pasted links, clipboard): only a still-running duplicate is reused,
// re-downloading a finished file on purpose is allowed.
func (s *Store) Add(r NewDownloadRequest) *DownloadItem {
	it, _ := s.addDedup(r, false)
	return it
}

// findDuplicateLocked returns an item for the same link that is still running, or that finished
// within the last 30 minutes and whose file still exists (repeated clicks on a download button).
func (s *Store) findDuplicateLocked(kind, key string) (*DownloadItem, string) {
	for _, it := range s.items {
		if it.Kind != kind || (it.URL != key && it.SourceURL != key) {
			continue
		}
		if isActiveState(it.State) || it.State == StQueued {
			return it, "active"
		}
		if it.State == StCompleted && it.CompletedAt != nil && time.Since(*it.CompletedAt) < 30*time.Minute &&
			it.FinalPath != "" && fileExists(it.FinalPath) {
			return it, "done"
		}
	}
	return nil, ""
}

// AddDedup adds a download unless the same link is already running/just finished; dup is
// "active" or "done" when the existing item was returned instead.
func (s *Store) AddDedup(r NewDownloadRequest) (*DownloadItem, string) { return s.addDedup(r, true) }

func (s *Store) addDedup(r NewDownloadRequest, reuseDone bool) (*DownloadItem, string) {
	{
		key := r.SourceURL
		if key == "" {
			key = r.URL.String()
		}
		kind := r.Kind
		if kind != "video" {
			kind = "file"
		}
		s.mu.Lock()
		if d, why := s.findDuplicateLocked(kind, key); d != nil && (reuseDone || why == "active") {
			cp := *d
			s.mu.Unlock()
			return &cp, why
		}
		s.mu.Unlock()
	}
	st := s.settings.Get()
	headers := map[string]string{}
	for k, v := range r.ExtraHeaders {
		headers[k] = v
	}
	if r.Referer != "" {
		headers["Referer"] = r.Referer
	}
	if r.UserAgent != "" {
		headers["User-Agent"] = r.UserAgent
	}
	if r.CookieHeader != "" {
		headers["Cookie"] = r.CookieHeader
	}
	if headers["User-Agent"] == "" {
		headers["User-Agent"] = defaultUserAgent
	}
	kind := r.Kind
	if kind != "video" {
		kind = "file"
	}
	feedHint := ""
	if kind == "video" {
		r.URL = normalizeVideoURL(r.URL)
		feedHint = feedPageHint(r.URL)
	}
	name := r.FileName
	if name == "" {
		name = fileNameFromURL(r.URL)
	}
	if kind == "video" {
		name = r.URL.Hostname()
		if name == "" {
			name = "video"
		}
	}
	it := &DownloadItem{
		ID: newID(), URL: r.URL.String(), FileName: sanitizeName(name), Directory: st.DownloadDirectory,
		Kind: kind, State: StQueued, TotalBytes: -1, Headers: headers, CreatedAt: time.Now(),
		RequestedSegments: r.Segments, Section: r.Section, SourceURL: r.SourceURL,
	}
	if feedHint != "" {
		it.State, it.ErrorMessage = StFailed, feedHint
	}
	if kind == "video" && len(r.Cookies) > 0 {
		it.CookieFile = writeCookieFile(r.Cookies, it.ID)
	}
	_ = os.MkdirAll(it.Directory, 0o755)
	s.mu.Lock()
	s.items = append([]*DownloadItem{it}, s.items...)
	s.scheduleSaveLocked()
	if st.AutoStart {
		s.pumpLocked()
	}
	cp := *it
	s.mu.Unlock()
	s.activityChanged()
	return &cp, ""
}

var reSection = regexp.MustCompile(`^\s*(\d{1,3}(:\d{1,2}){0,2}(\.\d+)?)\s*-\s*(\d{1,3}(:\d{1,2}){0,2}(\.\d+)?|inf)\s*$`)

// normalizeSection validates a "start-end" time range (end may be "inf"); empty if invalid.
func normalizeSection(s string) string {
	m := reSection.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return m[1] + "-" + m[4]
}

func (s *Store) AddURLs(text, kind, section string) int {
	section = normalizeSection(section)
	if kind != "video" {
		section = ""
	}
	n := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		u, err := url.Parse(line)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		s.Add(NewDownloadRequest{URL: u, Kind: kind, Section: section})
		n++
	}
	return n
}

// ---------------------------------------------------------------------------------------------
// Queue

func (s *Store) pumpLocked() {
	slots := s.settings.Get().MaxConcurrent - s.activeCountLocked()
	for i := len(s.items) - 1; i >= 0 && slots > 0; i-- {
		if s.items[i].State == StQueued {
			s.startLocked(s.items[i])
			slots--
		}
	}
	if s.activeCountLocked() > 0 {
		s.ensureTickerLocked()
	}
}

func (s *Store) startLocked(it *DownloadItem) {
	if it.Kind == "video" {
		s.startVideoLocked(it)
	} else {
		s.startFileLocked(it)
	}
}

func (s *Store) startFileLocked(it *DownloadItem) {
	u, err := url.Parse(it.URL)
	if err != nil {
		it.State, it.ErrorMessage = StFailed, err.Error()
		return
	}
	resumable := it.SupportsRanges && it.TotalBytes > 0 && len(it.Segments) > 0 && fileExists(it.partPath())
	segCount := it.RequestedSegments
	if segCount <= 0 {
		segCount = s.settings.Get().SegmentCount
	}
	id := it.ID
	var dl *SegmentedDownloader
	cfg := SegConfig{URL: u, Headers: it.Headers, PartPath: it.partPath(), SegmentCount: segCount}
	if resumable {
		cfg.ResumeSegs = append([]Segment(nil), it.Segments...)
		cfg.ResumeTotal = it.TotalBytes
	}
	cfg.OnProbe = func(p ProbeResult) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.downloaders[id] != dl {
			return
		}
		s.handleProbeLocked(id, p)
	}
	cfg.OnFinish = func(err error) {
		s.mu.Lock()
		if s.downloaders[id] != dl {
			s.mu.Unlock()
			return
		}
		s.handleFinishLocked(id, dl, err)
		s.mu.Unlock()
		s.activityChanged()
	}
	dl = NewSegmentedDownloader(cfg)
	s.downloaders[id] = dl
	it.ErrorMessage = ""
	it.Speed = 0
	if resumable {
		it.State = StDownloading
	} else {
		it.State = StProbing
		it.ReceivedBytes = 0
		it.Segments = nil
	}
	dl.Start()
}

func (s *Store) handleProbeLocked(id string, p ProbeResult) {
	it := s.get(id)
	if it == nil {
		return
	}
	it.TotalBytes = p.Total
	it.SupportsRanges = p.Ranges
	it.State = StDownloading
	orig, _ := url.Parse(it.URL)
	if p.SuggestedName != "" {
		it.FileName = sanitizeName(p.SuggestedName)
	} else if p.FinalURL != nil && orig != nil && it.FileName == sanitizeName(fileNameFromURL(orig)) && p.FinalURL.String() != it.URL {
		if c := fileNameFromURL(p.FinalURL); filepath.Ext(c) != "" {
			it.FileName = sanitizeName(c)
		}
	}
	if filepath.Ext(it.FileName) == "" && p.ContentType != "" {
		if ext := extForMIME(p.ContentType); ext != "" {
			it.FileName += ext
		}
	}
}

var preferredExt = map[string]string{
	"application/zip": ".zip", "application/pdf": ".pdf", "video/mp4": ".mp4", "audio/mpeg": ".mp3",
	"image/jpeg": ".jpg", "image/png": ".png", "application/x-msdownload": ".exe", "application/x-7z-compressed": ".7z",
	"application/vnd.rar": ".rar", "application/x-rar-compressed": ".rar", "video/webm": ".webm", "text/plain": ".txt",
	"application/x-msi": ".msi", "application/gzip": ".gz", "audio/mp4": ".m4a", "video/quicktime": ".mov",
}

func extForMIME(ct string) string {
	ct = strings.ToLower(ct)
	if e, ok := preferredExt[ct]; ok {
		return e
	}
	if ct == "application/octet-stream" {
		return ""
	}
	if exts, _ := mime.ExtensionsByType(ct); len(exts) > 0 {
		return exts[0]
	}
	return ""
}

func (s *Store) handleFinishLocked(id string, dl *SegmentedDownloader, err error) {
	delete(s.downloaders, id)
	it := s.get(id)
	if it == nil {
		return
	}
	recv, segs := dl.Snapshot()
	it.ReceivedBytes = recv
	if segs != nil {
		it.Segments = segs
	}
	if err != nil {
		if it.State != StPaused && it.State != StCancelled {
			it.State, it.ErrorMessage, it.Speed = StFailed, err.Error(), 0
			go notify(T("notifFailed"), it.FileName, "")
		}
	} else {
		dir := s.destDir(it)
		_ = os.MkdirAll(dir, 0o755)
		dest := uniqueDestination(filepath.Join(dir, it.FileName))
		if err := os.Rename(it.partPath(), dest); err != nil {
			it.State, it.ErrorMessage = StFailed, err.Error()
		} else {
			unhideFile(dest)
			applyMarkOfTheWeb(dest, it.URL, it.Headers["Referer"])
			now := time.Now()
			it.FileName, it.FinalPath, it.State, it.CompletedAt, it.Speed = filepath.Base(dest), dest, StCompleted, &now, 0
			it.Segments = nil
			if it.TotalBytes <= 0 {
				it.TotalBytes = it.ReceivedBytes
			}
			if s.settings.Get().NotifyOnComplete {
				go notify(T("notifDone"), it.FileName, dest)
			}
		}
	}
	s.scheduleSaveLocked()
	s.pumpLocked()
}

// ---------------------------------------------------------------------------------------------
// Video (yt-dlp)

func (s *Store) startVideoLocked(it *DownloadItem) {
	id := it.ID
	st := s.settings.Get()
	exe := locateYtDlp(st.YtdlpPath)
	if exe == "" {
		it.State, it.ErrorMessage = StFailed, T("errNoYtdlp")
		return
	}
	u, err := url.Parse(it.URL)
	if err != nil {
		it.State, it.ErrorMessage = StFailed, err.Error()
		return
	}
	if isYouTube(u) && locateDeno() == "" && !st.DenoPromptOff && !s.denoDeclined {
		it.State, it.ErrorMessage, it.Speed, it.Stage = StDownloading, "", 0, T("stageDenoWait")
		if !contains(s.denoWaiters, id) {
			s.denoWaiters = append(s.denoWaiters, id)
		}
		if !s.denoPrompting {
			s.denoPrompting = true
			go s.promptForDeno()
		}
		return
	}
	useCookies := !s.cookieless[id]
	it.Live, it.Stopping, it.LastDest, it.Started = false, false, "", time.Now()
	it.State, it.ErrorMessage, it.ReceivedBytes, it.Speed, it.TotalBytes = StDownloading, "", 0, 0, -1
	it.Stage = T("stageInfo")
	if useCookies {
		it.Log = ""
	}
	outDir := s.destDir(it)
	_ = os.MkdirAll(outDir, 0o755)
	s.phases[id] = &videoPhase{approx: -1}
	var job *VideoJob
	job, err = runVideo(VideoRunOpts{
		Exe: exe, URL: u, OutDir: outDir, FormatArgs: st.YtdlpFormatArgs(locateFfmpeg() != ""),
		Headers: it.Headers, CookieFile: it.CookieFile, UseCookies: useCookies, RateLimit: st.SpeedLimitBytes(),
		LiveFromStart: st.LiveFromStart && it.Section == "" && liveFromStartHost(u), Section: it.Section,
		OnProgress: func(p VideoProgress) {
			s.mu.Lock()
			defer s.mu.Unlock()
			ph := s.phases[id]
			it := s.get(id)
			if ph == nil || it == nil {
				return
			}
			if p.Downloaded < ph.last {
				ph.base += ph.last // next file (audio after video)
			}
			ph.last = p.Downloaded
			recv := ph.base + p.Downloaded
			it.ReceivedBytes = recv
			phaseTotal := int64(-1)
			if p.Total > 0 {
				phaseTotal = ph.base + p.Total
			}
			it.TotalBytes = max(ph.approx, phaseTotal, recv)
			it.Speed = p.Speed
		},
		OnLine: func(line string) {
			s.mu.Lock()
			defer s.mu.Unlock()
			it := s.get(id)
			if it == nil {
				return
			}
			it.Log += line + "\n"
			if len(it.Log) > 6000 {
				it.Log = trimUTF8Front(it.Log, 6000)
			}
			if stg := videoStage(line); stg != "" {
				it.Stage = stg
			}
			if strings.HasPrefix(line, "MCFMT|") {
				p := strings.Split(line, "|")
				if len(p) > 8 && p[8] == "True" {
					it.Live = true
				}
				if len(p) > 5 {
					var n float64
					if _, err := fmt.Sscan(p[5], &n); err == nil && n > 0 {
						if ph := s.phases[id]; ph != nil {
							ph.approx = int64(n)
						}
						if it.TotalBytes < int64(n) {
							it.TotalBytes = int64(n)
						}
					}
				}
			}
			if i := strings.Index(line, "Destination: "); i >= 0 {
				it.LastDest = strings.TrimSpace(line[i+len("Destination: "):])
				it.FileName = cleanVideoName(it.LastDest)
			}
			if strings.HasPrefix(line, `[Merger] Merging formats into "`) {
				q1, q2 := strings.Index(line, `"`), strings.LastIndex(line, `"`)
				if q1 < q2 {
					it.FileName = filepath.Base(line[q1+1 : q2])
				}
			}
		},
		OnFinish: func(fp string, err error) {
			s.mu.Lock()
			if s.videos[id] != job {
				s.mu.Unlock()
				return
			}
			s.handleVideoFinishLocked(id, fp, err)
			s.mu.Unlock()
			s.activityChanged()
		},
	})
	if err != nil {
		it.State, it.ErrorMessage = StFailed, err.Error()
		delete(s.phases, id)
		return
	}
	s.videos[id] = job
}

func (s *Store) handleVideoFinishLocked(id, fp string, err error) {
	delete(s.videos, id)
	delete(s.phases, id)
	it := s.get(id)
	if it == nil || it.State != StDownloading {
		return
	}
	if err != nil && it.Stopping {
		// The recording was stopped by the user but yt-dlp did not finalize (it was killed):
		// keep what was recorded. Live HLS is written as MPEG-TS, so the .part plays as .ts.
		if p := rescueLivePart(it.LastDest); p != "" {
			fp, err = p, nil
		}
	}
	it.Stopping = false
	if err == nil {
		now := time.Now()
		it.State, it.CompletedAt, it.Speed, it.Stage = StCompleted, &now, 0, ""
		if fp != "" {
			it.FinalPath = fp
			it.FileName = filepath.Base(fp)
			applyMarkOfTheWeb(fp, it.URL, it.Headers["Referer"])
			if fi, e := os.Stat(fp); e == nil {
				it.TotalBytes, it.ReceivedBytes = fi.Size(), fi.Size()
			}
		} else if it.TotalBytes > 0 {
			it.ReceivedBytes = it.TotalBytes
		}
		if s.settings.Get().NotifyOnComplete {
			go notify(T("notifDone"), it.FileName, fp)
		}
	} else {
		u, _ := url.Parse(it.URL)
		if !s.cookieless[id] && u != nil && isYouTube(u) && isSignedInClientFailure(err.Error()) && mergedCookieFile(it.CookieFile) != "" {
			s.cookieless[id] = true
			it.Log += T("retryNoCookies") + "\n"
			s.startVideoLocked(it)
			return
		}
		it.State, it.ErrorMessage, it.Speed = StFailed, friendlyVideoError(err.Error(), mergedCookieFile(it.CookieFile) != ""), 0
		go notify(T("notifFailed"), it.FileName, "")
	}
	s.scheduleSaveLocked()
	s.pumpLocked()
}

func (s *Store) activeVideoCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.videos)
}

func (s *Store) promptForDeno() {
	yes, never := askYesNoNever(T("denoTitle"), T("denoBody"))
	if never {
		s.settings.Update(func(st *Settings) { st.DenoPromptOff = true })
	}
	if !yes {
		s.mu.Lock()
		s.denoDeclined = true
		s.mu.Unlock()
		s.resumeDenoWaiters()
		return
	}
	s.mu.Lock()
	for _, id := range s.denoWaiters {
		if it := s.get(id); it != nil {
			it.Stage = T("stageDenoInstall")
		}
	}
	s.mu.Unlock()
	if err := tools.Install("deno"); err != nil {
		s.mu.Lock()
		s.denoDeclined = true
		for _, id := range s.denoWaiters {
			if it := s.get(id); it != nil {
				it.Log += "deno: " + err.Error() + "\n"
			}
		}
		s.mu.Unlock()
	}
	s.resumeDenoWaiters()
}

func (s *Store) resumeDenoWaiters() {
	s.mu.Lock()
	s.denoPrompting = false
	waiting := s.denoWaiters
	s.denoWaiters = nil
	for _, id := range waiting {
		if it := s.get(id); it != nil && it.State == StDownloading {
			s.startVideoLocked(it)
		}
	}
	s.mu.Unlock()
	s.activityChanged()
}

// ---------------------------------------------------------------------------------------------
// User actions

func (s *Store) stopWorkersLocked(id string) {
	if dl := s.downloaders[id]; dl != nil {
		dl.Stop()
		recv, segs := dl.Snapshot()
		if it := s.get(id); it != nil {
			it.ReceivedBytes = recv
			if segs != nil {
				it.Segments = segs
			}
		}
		delete(s.downloaders, id)
	}
	if j := s.videos[id]; j != nil {
		delete(s.videos, id)
		j.Stop()
	}
	delete(s.phases, id)
	delete(s.samples, id)
}

func (s *Store) Pause(id string) {
	s.mu.Lock()
	it := s.get(id)
	if it == nil || !(isActiveState(it.State) || it.State == StQueued) {
		s.mu.Unlock()
		return
	}
	it.State, it.Speed = StPaused, 0
	s.stopWorkersLocked(id)
	s.scheduleSaveLocked()
	s.pumpLocked()
	s.mu.Unlock()
	s.activityChanged()
}

// StopRecording ends a running yt-dlp download gracefully and keeps what was recorded
// (meant for live streams; for normal videos it ends the download early).
func (s *Store) StopRecording(id string) {
	s.mu.Lock()
	it := s.get(id)
	job := s.videos[id]
	if it == nil || job == nil || it.Stopping || it.State != StDownloading {
		s.mu.Unlock()
		return
	}
	it.Stopping = true
	it.Stage = T("stageStopping")
	s.mu.Unlock()
	if !job.Interrupt() {
		job.Stop()
		return
	}
	// yt-dlp needs a moment to flush and remux; force it if it hangs.
	time.AfterFunc(45*time.Second, func() {
		s.mu.Lock()
		still := s.videos[id] == job
		s.mu.Unlock()
		if still {
			job.Stop()
		}
	})
}

func liveFromStartHost(u *url.URL) bool {
	h := strings.ToLower(u.Hostname())
	return isYouTube(u) || h == "twitch.tv" || strings.HasSuffix(h, ".twitch.tv")
}

func rescueLivePart(dest string) string {
	if dest == "" {
		return ""
	}
	part := dest + ".part"
	if fi, err := os.Stat(part); err != nil || fi.Size() == 0 {
		if fileExists(dest) {
			return dest
		}
		return ""
	}
	target := uniqueDestination(strings.TrimSuffix(dest, filepath.Ext(dest)) + ".ts")
	if os.Rename(part, target) != nil {
		return ""
	}
	return target
}

func (s *Store) Resume(id string) {
	s.mu.Lock()
	it := s.get(id)
	if it == nil || !(it.State == StPaused || it.State == StFailed || it.State == StCancelled) {
		s.mu.Unlock()
		return
	}
	it.State, it.ErrorMessage = StQueued, ""
	delete(s.cookieless, id)
	s.pumpLocked()
	s.scheduleSaveLocked()
	s.mu.Unlock()
	s.activityChanged()
}

func (s *Store) Cancel(id string) {
	s.mu.Lock()
	it := s.get(id)
	if it == nil {
		s.mu.Unlock()
		return
	}
	s.stopWorkersLocked(id)
	it.State, it.Speed, it.ReceivedBytes, it.Segments = StCancelled, 0, 0, nil
	part := it.partPath()
	s.scheduleSaveLocked()
	s.pumpLocked()
	s.mu.Unlock()
	removeLater(part)
	s.activityChanged()
}

func (s *Store) Remove(id string, deleteFile bool) {
	s.mu.Lock()
	i := s.index(id)
	if i < 0 {
		s.mu.Unlock()
		return
	}
	it := s.items[i]
	s.stopWorkersLocked(id)
	s.items = append(s.items[:i], s.items[i+1:]...)
	part, cookie, final := it.partPath(), it.CookieFile, it.FinalPath
	s.scheduleSaveLocked()
	s.pumpLocked()
	s.mu.Unlock()
	removeLater(part)
	if cookie != "" {
		_ = os.Remove(cookie)
	}
	if deleteFile && final != "" && fileExists(final) {
		moveToRecycleBin(final)
	}
	s.activityChanged()
}

// removeLater deletes a part file once the downloader goroutines have released it.
func removeLater(p string) {
	go func() {
		for i := 0; i < 20; i++ {
			if err := os.Remove(p); err == nil || os.IsNotExist(err) {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
	}()
}

func (s *Store) ids(filter func(*DownloadItem) bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, it := range s.items {
		if filter(it) {
			out = append(out, it.ID)
		}
	}
	return out
}

func (s *Store) PauseAll() {
	for _, id := range s.ids(func(it *DownloadItem) bool { return isActiveState(it.State) || it.State == StQueued }) {
		s.Pause(id)
	}
}

func (s *Store) ResumeAll() {
	for _, id := range s.ids(func(it *DownloadItem) bool { return it.State == StPaused || it.State == StFailed }) {
		s.Resume(id)
	}
}

func (s *Store) ClearFinished() {
	for _, id := range s.ids(func(it *DownloadItem) bool { return it.State == StCompleted || it.State == StCancelled }) {
		s.Remove(id, false)
	}
}

// StartQueued starts queued items (used when auto-start is off).
func (s *Store) StartQueued() {
	s.mu.Lock()
	s.pumpLocked()
	s.mu.Unlock()
	s.activityChanged()
}

func (s *Store) PathFor(id string) (final, dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.get(id)
	if it == nil {
		return "", ""
	}
	return it.FinalPath, s.destDir(it)
}

func (s *Store) Shutdown() {
	s.mu.Lock()
	for id := range s.downloaders {
		s.stopWorkersLocked(id)
	}
	for id := range s.videos {
		s.stopWorkersLocked(id)
	}
	s.mu.Unlock()
	s.save()
}

// ---------------------------------------------------------------------------------------------
// Ticker: runs only while something is downloading. Idle = no timers, no wakeups.

func (s *Store) ensureTickerLocked() {
	if s.tickerOn {
		return
	}
	s.tickerOn = true
	go s.tickLoop()
}

func (s *Store) tickLoop() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		active := s.tickLocked()
		if active == 0 {
			s.tickerOn = false
			s.mu.Unlock()
			s.save()
			s.activityChanged()
			return
		}
		s.mu.Unlock()
		s.activityChanged()
	}
}

func (s *Store) tickLocked() int {
	now := time.Now()
	activeIDs := map[string]bool{}
	for _, it := range s.items {
		if !isActiveState(it.State) {
			continue
		}
		if dl := s.downloaders[it.ID]; dl != nil {
			recv, segs := dl.Snapshot()
			it.ReceivedBytes = recv
			if segs != nil {
				it.Segments = segs
			}
			if it.State == StProbing && recv > 0 {
				it.State = StDownloading
			}
		}
		activeIDs[it.ID] = true
		if it.Kind == "video" {
			continue
		}
		if sm, ok := s.samples[it.ID]; ok {
			if dt := now.Sub(sm.t).Seconds(); dt > 0 {
				inst := float64(it.ReceivedBytes-sm.bytes) / dt
				if inst < 0 {
					inst = 0
				}
				if it.Speed == 0 {
					it.Speed = inst
				} else {
					it.Speed = it.Speed*0.6 + inst*0.4
				}
			}
		}
		s.samples[it.ID] = speedSample{it.ReceivedBytes, now}
	}
	for id := range s.samples {
		if !activeIDs[id] {
			delete(s.samples, id)
		}
	}
	s.tickCount++
	if len(activeIDs) > 0 && s.tickCount%10 == 0 {
		go s.save() // segment table to disk every 5 s for crash-safe resume
	}
	return len(activeIDs)
}

func (s *Store) activityChanged() {
	if s.OnActivity == nil {
		return
	}
	s.mu.Lock()
	active := 0
	var total, recv int64
	var speed float64
	for _, it := range s.items {
		if isActiveState(it.State) {
			active++
			speed += it.Speed
			if it.TotalBytes > 0 {
				total += it.TotalBytes
				recv += min(it.ReceivedBytes, it.TotalBytes)
			}
		}
	}
	s.mu.Unlock()
	overall := -1.0
	if total > 0 {
		overall = float64(recv) / float64(total)
	}
	s.OnActivity(active, overall, speed)
}

// ---------------------------------------------------------------------------------------------
// Persistence

func (s *Store) load() {
	b, err := os.ReadFile(s.storePath)
	if err != nil {
		return
	}
	var arr []*DownloadItem
	if json.Unmarshal(b, &arr) != nil {
		return
	}
	for _, it := range arr {
		if isActiveState(it.State) || it.State == StQueued {
			it.State = StPaused
		}
	}
	s.items = arr
	s.cleanupOrphanParts()
}

func (s *Store) cleanupOrphanParts() {
	dir := s.settings.Get().DownloadDirectory
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	known := map[string]bool{}
	for _, it := range s.items {
		known[".dm-"+it.ID+".part"] = true
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".dm-") && strings.HasSuffix(n, ".part") && !known[n] {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
}

func (s *Store) save() {
	s.mu.Lock()
	items := s.items
	if items == nil {
		items = []*DownloadItem{}
	}
	b, err := json.Marshal(items)
	s.mu.Unlock()
	if err == nil {
		writeFileAtomic(s.storePath, b)
	}
}

func (s *Store) scheduleSaveLocked() {
	if s.saveTimer != nil {
		s.saveTimer.Stop()
	}
	s.saveTimer = time.AfterFunc(500*time.Millisecond, s.save)
}

// ---------------------------------------------------------------------------------------------
// Views for the API

type ItemView struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	URL       string  `json:"url"`
	Kind      string  `json:"kind"`
	State     string  `json:"state"`
	Progress  float64 `json:"progress"`
	Speed     float64 `json:"speed"`
	Total     int64   `json:"total"`
	Received  int64   `json:"received"`
	Stage     string  `json:"stage"`
	Error     string  `json:"error,omitempty"`
	Category  string  `json:"category"`
	Segments  int     `json:"segments"`
	Created   int64   `json:"created"`
	Completed int64   `json:"completed,omitempty"`
	HasFile   bool    `json:"hasFile"`
	Log       string  `json:"log,omitempty"`
	Live      bool    `json:"live,omitempty"`
	Stopping  bool    `json:"stopping,omitempty"`
	Section   string  `json:"section,omitempty"`
	Elapsed   int64   `json:"elapsed,omitempty"` // seconds recorded so far (live)
}

func (s *Store) Views(limit int, withLog bool) []ItemView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ItemView, 0, min(limit, len(s.items)))
	for i, it := range s.items {
		if i >= limit {
			break
		}
		v := ItemView{
			ID: it.ID, Name: it.FileName, URL: it.URL, Kind: it.Kind, State: it.State, Progress: it.progress(),
			Speed: it.Speed, Total: it.TotalBytes, Received: it.ReceivedBytes, Stage: it.Stage, Error: it.ErrorMessage,
			Category: categoryOf(it.FileName, it.Kind), Segments: len(it.Segments), Created: it.CreatedAt.UnixMilli(),
			HasFile: it.FinalPath != "", Live: it.Live, Stopping: it.Stopping, Section: it.Section,
		}
		if it.Live && isActiveState(it.State) && !it.Started.IsZero() {
			v.Elapsed = int64(time.Since(it.Started).Seconds())
		}
		if it.CompletedAt != nil {
			v.Completed = it.CompletedAt.UnixMilli()
		}
		if withLog {
			v.Log = it.Log
		}
		out = append(out, v)
	}
	return out
}

func (s *Store) Log(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if it := s.get(id); it != nil {
		return it.Log
	}
	return ""
}

// ---------------------------------------------------------------------------------------------
// Helpers

func writeCookieFile(cookies []map[string]any, id string) string {
	lines := []string{"# Netscape HTTP Cookie File"}
	for _, c := range cookies {
		name, _ := c["name"].(string)
		value, _ := c["value"].(string)
		domain, _ := c["domain"].(string)
		if name == "" || domain == "" {
			continue
		}
		p, _ := c["path"].(string)
		if p == "" {
			p = "/"
		}
		secure := "FALSE"
		if b, _ := c["secure"].(bool); b {
			secure = "TRUE"
		}
		exp, _ := c["expirationDate"].(float64)
		flag := "FALSE"
		if strings.HasPrefix(domain, ".") {
			flag = "TRUE"
		}
		clean := func(x string) string { return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(x) }
		lines = append(lines, strings.Join([]string{clean(domain), flag, clean(p), secure, fmt.Sprint(int64(exp)), clean(name), clean(value)}, "\t"))
	}
	dir := filepath.Join(supportDir(), "cookies")
	_ = os.MkdirAll(dir, 0o700)
	f := filepath.Join(dir, id+".txt")
	if err := os.WriteFile(f, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return ""
	}
	return f
}

func fileNameFromURL(u *url.URL) string {
	n := path.Base(u.Path)
	if dec, err := url.PathUnescape(n); err == nil {
		n = dec
	}
	if n == "" || n == "/" || n == "." {
		n = u.Hostname()
	}
	if n == "" {
		n = "download"
	}
	return n
}

var reservedNames = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true}

// sanitizeName makes a string safe as a Windows file name.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 32 || strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	n := strings.TrimSpace(b.String())
	n = strings.TrimRight(n, ". ")
	if strings.HasPrefix(n, ".") {
		n = "_" + n
	}
	base := strings.ToUpper(strings.TrimSuffix(n, filepath.Ext(n)))
	if reservedNames[base] {
		n = "_" + n
	}
	if utf8.RuneCountInString(n) > 200 {
		ext := filepath.Ext(n)
		if utf8.RuneCountInString(ext) > 16 {
			ext = ""
		}
		r := []rune(strings.TrimSuffix(n, ext))
		n = string(r[:180]) + ext
	}
	if n == "" {
		return "download"
	}
	return n
}

func uniqueDestination(p string) string {
	if !fileExists(p) {
		return p
	}
	dir, ext := filepath.Dir(p), filepath.Ext(p)
	base := strings.TrimSuffix(filepath.Base(p), ext)
	for n := 2; ; n++ {
		c := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, n, ext))
		if !fileExists(c) {
			return c
		}
	}
}

func trimUTF8Front(s string, maxLen int) string {
	cut := len(s) - maxLen
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}

// applyMarkOfTheWeb tags a downloaded file like a browser download (Zone.Identifier stream),
// so SmartScreen / Office Protected View assess it on first open.
func applyMarkOfTheWeb(p, src, referer string) {
	if !isWindows {
		return
	}
	body := "[ZoneTransfer]\r\nZoneId=3\r\n"
	if referer != "" && !strings.ContainsAny(referer, "\r\n") {
		body += "ReferrerUrl=" + referer + "\r\n"
	}
	if !strings.ContainsAny(src, "\r\n") {
		body += "HostUrl=" + src + "\r\n"
	}
	_ = os.WriteFile(p+":Zone.Identifier", []byte(body), 0o644)
}
