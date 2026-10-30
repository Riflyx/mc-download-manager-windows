package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const exeSuffix = ".exe"

func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(p)
}

func isExecutable(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func toolName(n string) string {
	if isWindows {
		return n + exeSuffix
	}
	return n
}

func locateTool(name string, extra ...string) string {
	cands := append([]string{}, extra...)
	cands = append(cands,
		filepath.Join(binDir(), toolName(name)),
		filepath.Join(exeDir(), toolName(name)),
		filepath.Join(exeDir(), "tools", toolName(name)),
	)
	for _, c := range cands {
		if isExecutable(c) {
			return c
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	return ""
}

func locateYtDlp(custom string) string { return locateTool("yt-dlp", custom) }
func locateFfmpeg() string             { return locateTool("ffmpeg") }
func locateDeno() string {
	home, _ := os.UserHomeDir()
	return locateTool("deno", filepath.Join(home, ".deno", "bin", toolName("deno")))
}

func isYouTube(u *url.URL) bool {
	h := strings.ToLower(u.Hostname())
	for _, d := range []string{"youtube.com", "youtu.be", "youtube-nocookie.com"} {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

func isSignedInClientFailure(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "page needs to be reloaded")
}

type VideoProgress struct {
	Downloaded, Total int64
	Speed             float64
}

type VideoJob struct {
	cmd  *exec.Cmd
	kill func()
}

// Interrupt asks yt-dlp to stop like Ctrl+C would. For live streams yt-dlp then finalizes what it has
// recorded so far (and remuxes it) instead of leaving a broken .part file.
func (j *VideoJob) Interrupt() bool {
	if j == nil || j.cmd == nil || j.cmd.Process == nil {
		return false
	}
	return interruptProcess(j.cmd.Process.Pid)
}

func (j *VideoJob) Stop() {
	if j == nil {
		return
	}
	if j.kill != nil {
		j.kill()
	} else if j.cmd != nil && j.cmd.Process != nil {
		_ = j.cmd.Process.Kill()
	}
}

var (
	reAudioStage = regexp.MustCompile(`\.f[0-9]+[-a-z0-9]*\.(m4a|webm|opus|mp3|aac|ogg)$`)
	reVideoStage = regexp.MustCompile(`\.f[0-9]+[-a-z0-9]*\.[a-z0-9]+$`)
	reFmtSuffix  = regexp.MustCompile(`\.f[0-9]+[-a-z0-9]*(\.[a-z0-9]+)$`)
)

func videoStage(line string) string {
	switch {
	case strings.HasPrefix(line, "[download] Destination:"):
		name := strings.ToLower(strings.TrimPrefix(line, "[download] Destination: "))
		if reAudioStage.MatchString(name) {
			return T("stageAudio")
		}
		if reVideoStage.MatchString(name) {
			return T("stageVideo")
		}
		return T("stageDownloading")
	case strings.HasPrefix(line, "[Merger]"):
		return T("stageMerge")
	case strings.HasPrefix(line, "[VideoRemuxer]"), strings.HasPrefix(line, "[VideoConvertor]"):
		return T("stageRemux")
	case strings.HasPrefix(line, "[ExtractAudio]"):
		return T("stageExtract")
	case strings.HasPrefix(line, "MCFMT|"):
		p := strings.Split(line, "|")
		if len(p) < 6 {
			return ""
		}
		var parts []string
		if p[1] != "NA" && p[1] != "" {
			parts = append(parts, p[1])
		}
		if p[2] != "NA" {
			parts = append(parts, strings.ToUpper(p[2]))
		}
		if p[3] != "NA" && p[3] != "none" {
			parts = append(parts, strings.Split(p[3], ".")[0])
		}
		if n, err := strconv.ParseFloat(p[5], 64); err == nil && n > 0 {
			parts = append(parts, "~"+fmtSize(int64(n)))
		}
		return T("stageSelected") + strings.Join(parts, " · ")
	case strings.Contains(line, "has already been downloaded"):
		return T("stageAlready")
	}
	return ""
}

// cleanVideoName strips yt-dlp's intermediate ".f137" suffix from a destination file name.
func cleanVideoName(p string) string {
	return reFmtSuffix.ReplaceAllString(filepath.Base(p), "$1")
}

func mergedCookieFile(itemCookies string) string {
	if itemCookies != "" && fileExists(itemCookies) {
		return itemCookies
	}
	return ""
}

type VideoRunOpts struct {
	Exe           string
	URL           *url.URL
	OutDir        string
	FormatArgs    []string
	Headers       map[string]string
	CookieFile    string
	UseCookies    bool
	RateLimit     int64
	LiveFromStart bool
	Section       string // time range for --download-sections, e.g. "1:20:00-1:35:00"
	OnProgress    func(VideoProgress)
	OnLine        func(string)
	OnFinish      func(finalPath string, err error)
}

func runVideo(o VideoRunOpts) (*VideoJob, error) {
	args := []string{
		"--newline", "--no-playlist", "--no-colors", "--progress", "--no-simulate", "--no-mtime", "--no-quiet",
		"-N", "4",
		"-o", filepath.Join(o.OutDir, "%(title).150B [%(id)s].%(ext)s"),
		"--progress-template",
		"download:DMP|%(progress.downloaded_bytes)s|%(progress.total_bytes)s|%(progress.total_bytes_estimate)s|%(progress.speed)s",
		"--print", "after_move:filepath",
		"--print", "before_dl:MCFMT|%(resolution)s|%(ext)s|%(vcodec)s|%(acodec)s|%(filesize_approx)s|%(format_id)s|%(duration)s|%(is_live)s",
	}
	args = append(args, o.FormatArgs...)
	if ff := locateFfmpeg(); ff != "" {
		args = append(args, "--ffmpeg-location", ff)
	}
	if ua := o.Headers["User-Agent"]; ua != "" {
		args = append(args, "--user-agent", ua)
	}
	if ref := o.Headers["Referer"]; ref != "" {
		args = append(args, "--referer", ref)
	}
	if o.UseCookies {
		if cf := mergedCookieFile(o.CookieFile); cf != "" {
			args = append(args, "--cookies", cf)
		}
	}
	if deno := locateDeno(); deno != "" {
		args = append(args, "--js-runtimes", "deno:"+deno)
	}
	if o.LiveFromStart {
		args = append(args, "--live-from-start")
	}
	if o.Section != "" {
		args = append(args, "--download-sections", "*"+o.Section)
	}
	if o.RateLimit > 0 {
		args = append(args, "--limit-rate", strconv.FormatInt(o.RateLimit, 10))
	}
	// Frozen yt-dlp.exe ignores PYTHONUTF8 and writes the ANSI code page (cp1254: ğ/ı become invalid UTF-8); force UTF-8.
	args = append(args, "--encoding", "utf-8", "--", o.URL.String())

	cmd := exec.Command(o.Exe, args...)
	cmd.Dir = o.OutDir
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	cmd.SysProcAttr = childProcAttr()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	job := &VideoJob{cmd: cmd, kill: attachKillTree(cmd)}

	var mu sync.Mutex
	var finalPath, lastErr string
	handle := func(raw string, isErr bool) {
		line := strings.TrimSpace(raw)
		if line == "" {
			return
		}
		if strings.HasPrefix(line, "DMP|") {
			p := strings.Split(line, "|")
			if len(p) < 5 {
				return
			}
			num := func(s string) int64 {
				f, err := strconv.ParseFloat(s, 64)
				if err != nil {
					return -1
				}
				return int64(f)
			}
			dl := num(p[1])
			if dl < 0 {
				dl = 0
			}
			total := num(p[2])
			if total <= 0 {
				total = num(p[3])
			}
			sp, _ := strconv.ParseFloat(p[4], 64)
			if sp < 0 {
				sp = 0
			}
			o.OnProgress(VideoProgress{dl, total, sp})
			return
		}
		if filepath.IsAbs(line) && fileExists(line) {
			mu.Lock()
			finalPath = line
			mu.Unlock()
		}
		if isErr && strings.HasPrefix(line, "ERROR") {
			mu.Lock()
			lastErr = line
			mu.Unlock()
		}
		o.OnLine(line)
	}
	var wg sync.WaitGroup
	scan := func(r io.Reader, isErr bool) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			handle(sc.Text(), isErr)
		}
	}
	wg.Add(2)
	go scan(stdout, false)
	go scan(stderr, true)
	go func() {
		wg.Wait()
		err := cmd.Wait()
		mu.Lock()
		fp, le := finalPath, lastErr
		mu.Unlock()
		if err != nil {
			var ee *exec.ExitError
			msg := le
			if msg == "" {
				if errors.As(err, &ee) {
					msg = fmt.Sprintf("yt-dlp exited with code %d", ee.ExitCode())
				} else {
					msg = err.Error()
				}
			}
			o.OnFinish("", errors.New(msg))
			return
		}
		o.OnFinish(fp, nil)
	}()
	return job, nil
}
