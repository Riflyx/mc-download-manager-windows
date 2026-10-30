package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui/index.html
var uiHTML []byte

type Server struct {
	store    *Store
	settings *SettingsStore

	mu      sync.Mutex
	srv     *http.Server
	port    int
	status  string
	running bool
	conns   map[string]time.Time // browser -> last extension contact
	extVer  map[string]string    // browser -> extension version it reported
}

func NewServer(store *Store, settings *SettingsStore) *Server {
	return &Server{store: store, settings: settings, conns: map[string]time.Time{}, extVer: map[string]string{}}
}

func (s *Server) Start() error {
	port := s.settings.Get().ServerPort
	s.mu.Lock()
	old := s.srv
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		s.mu.Lock()
		s.running, s.port, s.status = false, port, err.Error()
		s.mu.Unlock()
		return err
	}
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	s.mu.Lock()
	s.srv, s.port, s.running, s.status = srv, port, true, fmt.Sprintf("127.0.0.1:%d", port)
	s.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	return nil
}

func (s *Server) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

func (s *Server) UIURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/ui/%s/", s.Port(), s.settings.Get().Token)
}

// ---------------------------------------------------------------------------------------------

func (s *Server) hostAllowed(host string) bool {
	host = strings.ToLower(host)
	p := strconv.Itoa(s.Port())
	return host == "127.0.0.1:"+p || host == "localhost:"+p || host == "[::1]:"+p
}

// Only browser extensions may drive the bridge (same rules as the macOS app).
func originAllowed(origin string) bool {
	if origin == "" || origin == "null" {
		return false
	}
	return strings.HasPrefix(origin, "chrome-extension://") ||
		strings.HasPrefix(origin, "moz-extension://") ||
		strings.HasPrefix(origin, "safari-web-extension://")
}

func extensionRequest(r *http.Request) bool {
	origin, hasOrigin := r.Header["Origin"]
	if hasOrigin && len(origin) > 0 {
		return originAllowed(origin[0])
	}
	return r.Header.Get("X-MCDM-Extension") != ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.hostAllowed(r.Host) {
		writeJSON(w, 403, map[string]any{"ok": false, "error": "forbidden"})
		return
	}
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/ui/"):
		s.serveUI(w, r)
	case strings.HasPrefix(p, "/api/"):
		s.serveAPI(w, r)
	case p == "/":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, appName+" "+appVersion+" is running. Open it from the tray icon.\n")
	default:
		s.serveBridge(w, r)
	}
}

// ---------------------------------------------------------------------------------------------
// Extension bridge (/ping, /downloads, /download, /action)

func (s *Server) serveBridge(w http.ResponseWriter, r *http.Request) {
	if !extensionRequest(r) {
		writeJSON(w, 403, map[string]any{"ok": false, "error": "forbidden"})
		return
	}
	if o := r.Header.Get("Origin"); originAllowed(o) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", o)
		h.Set("Vary", "Origin")
		h.Set("Access-Control-Allow-Headers", "Content-Type, X-MCDM-Extension, X-MCDM-Version")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Private-Network", "true")
		h.Set("Access-Control-Allow-Local-Network", "true")
	}
	if r.Method == http.MethodOptions {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	s.noteConnection(r)
	switch {
	case r.Method == "GET" && r.URL.Path == "/ping":
		st := s.settings.Get()
		writeJSON(w, 200, map[string]any{
			"ok": true, "app": "MCDownloadManager", "version": appVersion, "platform": "windows",
			"ytdlp": locateYtDlp(st.YtdlpPath) != "", "active": s.store.ActiveCount(),
			"captureExtensions": st.CaptureExtensions(),
		})
	case r.Method == "GET" && r.URL.Path == "/ui-url": // extension-only (checked above); the URL contains the UI token
		writeJSON(w, 200, map[string]any{"ok": true, "url": s.UIURL()})
	case r.Method == "GET" && r.URL.Path == "/downloads":
		views := s.store.Views(50, false)
		items := make([]map[string]any, 0, len(views))
		for _, v := range views {
			items = append(items, map[string]any{"id": v.ID, "name": v.Name, "state": v.State, "progress": v.Progress,
				"speed": v.Speed, "total": v.Total, "received": v.Received, "stage": v.Stage, "error": v.Error})
		}
		writeJSON(w, 200, map[string]any{"ok": true, "items": items})
	case r.Method == "POST" && r.URL.Path == "/download":
		var body struct {
			URL          string            `json:"url"`
			Filename     string            `json:"filename"`
			Referer      string            `json:"referer"`
			UserAgent    string            `json:"userAgent"`
			CookieHeader string            `json:"cookieHeader"`
			Cookies      []map[string]any  `json:"cookies"`
			Kind         string            `json:"kind"`
			Headers      map[string]string `json:"headers"`
			Segments     int               `json:"segments"`
			SourceURL    string            `json:"sourceUrl"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeJSON(w, 400, map[string]any{"ok": false, "error": "invalid json"})
			return
		}
		u, err := url.Parse(body.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			writeJSON(w, 400, map[string]any{"ok": false, "error": "invalid url"})
			return
		}
		src := ""
		if su, err := url.Parse(body.SourceURL); err == nil && (su.Scheme == "http" || su.Scheme == "https") {
			src = su.String()
		}
		it, dup := s.store.AddDedup(NewDownloadRequest{SourceURL: src, URL: u, FileName: body.Filename, Referer: body.Referer, UserAgent: body.UserAgent,
			CookieHeader: body.CookieHeader, Cookies: body.Cookies, Kind: body.Kind, ExtraHeaders: body.Headers,
			Segments: clamp(body.Segments, 0, 32)})
		writeJSON(w, 200, map[string]any{"ok": true, "id": it.ID, "name": it.FileName, "duplicate": dup})
	case r.Method == "POST" && r.URL.Path == "/action":
		var body struct{ ID, Action string }
		if err := decodeBody(r, &body); err != nil || body.ID == "" || body.Action == "" {
			writeJSON(w, 400, map[string]any{"ok": false, "error": "id and action required"})
			return
		}
		if err := s.doAction(body.ID, body.Action, false); err != nil {
			writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 404, map[string]any{"ok": false, "error": "not found"})
	}
}

func decodeBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(v)
}

func (s *Server) noteConnection(r *http.Request) {
	b := "other"
	if strings.HasPrefix(r.Header.Get("Origin"), "chrome-extension://") {
		b = "chrome"
	}
	switch h := strings.ToLower(r.Header.Get("X-MCDM-Extension")); h {
	case "chrome", "brave", "edge", "safari":
		b = h
	}
	if q := strings.ToLower(r.URL.Query().Get("browser")); q == "chrome" || q == "brave" || q == "edge" {
		b = q
	}
	s.mu.Lock()
	s.conns[b] = time.Now()
	if v := r.Header.Get("X-MCDM-Version"); v != "" && len(v) < 32 {
		s.extVer[b] = v
	}
	s.mu.Unlock()
}

func (s *Server) doAction(id, action string, allowLocal bool) error {
	switch action {
	case "pause":
		s.store.Pause(id)
	case "stop":
		s.store.StopRecording(id)
	case "resume":
		s.store.Resume(id)
	case "cancel":
		s.store.Cancel(id)
	case "remove":
		s.store.Remove(id, false)
	case "delete":
		if !allowLocal {
			return errors.New("unknown action")
		}
		s.store.Remove(id, true)
	case "open", "reveal":
		if !allowLocal && action != "reveal" { // the extension may show a file in Explorer, never open/run it
			return errors.New("unknown action")
		}
		final, dir := s.store.PathFor(id)
		if action == "open" && final != "" && fileExists(final) {
			shellOpen(final)
		} else if final != "" && fileExists(final) {
			revealInExplorer(final)
		} else if dir != "" {
			_ = mkdirAll(dir)
			shellOpen(dir)
		}
	default:
		return errors.New("unknown action")
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// Local web UI

func (s *Server) tokenOK(t string) bool {
	want := s.settings.Get().Token
	return t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(want)) == 1
}

func (s *Server) serveUI(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/ui/"), "/", 2)
	if !s.tokenOK(parts[0]) {
		http.Error(w, "forbidden — open the app from its tray icon", 403)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	_, _ = w.Write(uiHTML)
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r.Header.Get("X-MCDM-Token")) {
		writeJSON(w, 403, map[string]any{"ok": false, "error": "forbidden"})
		return
	}
	if o := r.Header.Get("Origin"); o != "" && !s.hostAllowed(strings.TrimPrefix(o, "http://")) {
		writeJSON(w, 403, map[string]any{"ok": false, "error": "forbidden"})
		return
	}
	switch r.URL.Path {
	case "/api/state":
		st := s.settings.Get()
		withLog := r.URL.Query().Get("log")
		views := s.store.Views(500, false)
		if withLog != "" {
			for i := range views {
				if views[i].ID == withLog {
					views[i].Log = s.store.Log(withLog)
				}
			}
		}
		s.mu.Lock()
		conns := map[string]int64{}
		vers := map[string]string{}
		for k, v := range s.extVer {
			vers[k] = v
		}
		for k, v := range s.conns {
			conns[k] = v.UnixMilli()
		}
		srvStatus, running := s.status, s.running
		s.mu.Unlock()
		groups := []map[string]any{}
		for _, g := range captureGroups {
			groups = append(groups, map[string]any{"id": g.ID, "exts": g.Exts})
		}
		writeJSON(w, 200, map[string]any{
			"ok": true, "version": appVersion, "items": views, "settings": st, "groups": groups,
			"server": map[string]any{"status": srvStatus, "running": running}, "connections": conns, "extVersions": vers, "extExpected": expectedExtVersion(),
			"now": time.Now().UnixMilli(), "lang": lang, "autostart": autostartEnabled(),
			"extensionPath": extensionDir(), "browsers": installedBrowsers(), "tools": tools.Status(st.YtdlpPath),
		})
	case "/api/add":
		var b struct{ URLs, Kind, Section string }
		if decodeBody(r, &b) != nil {
			writeJSON(w, 400, map[string]any{"ok": false})
			return
		}
		if strings.TrimSpace(b.Section) != "" && normalizeSection(b.Section) == "" {
			writeJSON(w, 400, map[string]any{"ok": false, "error": "badSection"})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "added": s.store.AddURLs(b.URLs, b.Kind, b.Section)})
	case "/api/action":
		var b struct{ ID, Action string }
		if decodeBody(r, &b) != nil {
			writeJSON(w, 400, map[string]any{"ok": false})
			return
		}
		if err := s.doAction(b.ID, b.Action, true); err != nil {
			writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case "/api/bulk":
		var b struct{ Action string }
		_ = decodeBody(r, &b)
		switch b.Action {
		case "pauseAll":
			s.store.PauseAll()
		case "resumeAll":
			s.store.ResumeAll()
		case "clearFinished":
			s.store.ClearFinished()
		case "startQueued":
			s.store.StartQueued()
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case "/api/settings":
		var patch map[string]json.RawMessage
		if decodeBody(r, &patch) != nil {
			writeJSON(w, 400, map[string]any{"ok": false})
			return
		}
		delete(patch, "token")
		oldPort := s.settings.Get().ServerPort
		b, _ := json.Marshal(patch)
		ns := s.settings.Update(func(st *Settings) { _ = json.Unmarshal(b, st) })
		applyRuntimeSettings(ns)
		if ns.ServerPort != oldPort {
			// Reply first, then move the listener.
			writeJSON(w, 200, map[string]any{"ok": true, "restart": true, "url": fmt.Sprintf("http://127.0.0.1:%d/ui/%s/", ns.ServerPort, ns.Token)})
			go func() { time.Sleep(300 * time.Millisecond); _ = s.Start(); onServerRestarted() }()
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case "/api/tools/install":
		var b struct{ Tool string }
		_ = decodeBody(r, &b)
		if err := tools.InstallAsync(b.Tool); err != nil {
			writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case "/api/browse":
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Minute))
		cur := s.settings.Get().DownloadDirectory
		p := browseForFolder(cur)
		if p != "" {
			s.settings.Update(func(st *Settings) { st.DownloadDirectory = p })
		}
		writeJSON(w, 200, map[string]any{"ok": true, "path": p})
	case "/api/open":
		var b struct{ What string }
		_ = decodeBody(r, &b)
		switch b.What {
		case "downloads":
			d := s.settings.Get().DownloadDirectory
			_ = mkdirAll(d)
			shellOpen(d)
		case "extension":
			shellOpen(ensureExtensionFiles())
		case "chrome", "edge", "brave": // browser's extensions page; the UI has already copied the folder path to the clipboard
			ensureExtensionFiles()
			exe := browserExe(b.What)
			if exe == "" || launchBrowser(exe, b.What+"://extensions") != nil {
				writeJSON(w, 200, map[string]any{"ok": false})
				return
			}
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case "/api/autostart":
		var b struct{ On bool }
		_ = decodeBody(r, &b)
		err := setAutostart(b.On)
		writeJSON(w, 200, map[string]any{"ok": err == nil, "autostart": autostartEnabled()})
	default:
		writeJSON(w, 404, map[string]any{"ok": false, "error": "not found"})
	}
}

func installedBrowsers() []string {
	var out []string
	for _, b := range []string{"chrome", "edge", "brave"} {
		if browserExe(b) != "" {
			out = append(out, b)
		}
	}
	return out
}
