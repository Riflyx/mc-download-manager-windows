package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Segment struct {
	Start    int64 `json:"start"`
	End      int64 `json:"end"` // inclusive
	Received int64 `json:"received"`
}

func (s Segment) Length() int64 { return s.End - s.Start + 1 }
func (s Segment) Done() bool    { return s.Received >= s.Length() }

type ProbeResult struct {
	Total         int64
	Ranges        bool
	SuggestedName string
	ContentType   string
	FinalURL      *url.URL
}

type HTTPStatusError struct{ Status int }

func (e HTTPStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.Status) }

// ---------------------------------------------------------------------------------------------
// Shared HTTP client. One transport for everything keeps idle sockets/goroutines to a minimum;
// idle connections are dropped after 30 s so nothing lingers once downloads finish.

var credentialHeaders = []string{"Cookie", "Authorization", "Proxy-Authorization"}

var httpClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          16,
		MaxConnsPerHost:       32,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
		ReadBufferSize:        64 << 10,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		// Credentials only go to the originally requested host, never to a redirect target.
		if !sameOrigin(req.URL, via[0].URL) {
			for _, h := range credentialHeaders {
				req.Header.Del(h)
			}
		}
		return nil
	},
}

func sameOrigin(a, b *url.URL) bool {
	return a != nil && b != nil && strings.EqualFold(a.Hostname(), b.Hostname()) &&
		strings.EqualFold(a.Scheme, b.Scheme) && a.Port() == b.Port()
}

// ---------------------------------------------------------------------------------------------
// Global token-bucket rate limiter shared by all segment connections.

type rateLimiter struct {
	rate   atomic.Int64 // bytes/s, 0 = unlimited
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

var limiter = &rateLimiter{}

func (l *rateLimiter) SetRate(r int64) { l.rate.Store(r) }

func (l *rateLimiter) Wait(ctx context.Context, n int) error {
	rate := l.rate.Load()
	if rate <= 0 {
		return nil
	}
	l.mu.Lock()
	now := time.Now()
	if l.last.IsZero() {
		l.last = now
	}
	burst := float64(rate) / 4
	if burst < 32<<10 {
		burst = 32 << 10
	}
	l.tokens += now.Sub(l.last).Seconds() * float64(rate)
	if l.tokens > burst {
		l.tokens = burst
	}
	l.last = now
	l.tokens -= float64(n)
	var wait time.Duration
	if l.tokens < 0 {
		wait = time.Duration(-l.tokens / float64(rate) * float64(time.Second))
	}
	l.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 128<<10); return &b }}

// ---------------------------------------------------------------------------------------------
// SegmentedDownloader downloads one file over N parallel HTTP Range requests (port of the macOS engine).

type SegConfig struct {
	URL          *url.URL
	Headers      map[string]string
	PartPath     string
	SegmentCount int
	MinSegment   int64
	ResumeSegs   []Segment
	ResumeTotal  int64
	OnProbe      func(ProbeResult)
	OnFinish     func(error)
	IdleTimeout  time.Duration
	MaxAttempts  int
	Auto         bool // SegmentCount is a ceiling; the number of live connections is tuned while downloading
}

type SegmentedDownloader struct {
	cfg        SegConfig
	ctx        context.Context
	cancel     context.CancelFunc
	requestURL *url.URL

	mu         sync.Mutex
	segments   []Segment
	single     bool
	singleRecv atomic.Int64
	file       *os.File
	finishOnce sync.Once
	conns      atomic.Int32 // live connections / current target (for the detail view)
	target     atomic.Int32
}

func NewSegmentedDownloader(cfg SegConfig) *SegmentedDownloader {
	if cfg.MinSegment <= 0 {
		cfg.MinSegment = 512 << 10
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 30 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 12
	}
	if cfg.SegmentCount < 1 {
		cfg.SegmentCount = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &SegmentedDownloader{cfg: cfg, ctx: ctx, cancel: cancel, requestURL: cfg.URL}
}

func (d *SegmentedDownloader) Start() { go d.run() }

// Stop pauses/cancels. OnFinish is not called for a stop.
func (d *SegmentedDownloader) Stop() {
	d.finishOnce.Do(func() {})
	d.cancel()
}

// Snapshot returns bytes received and a copy of the segment table (for persistence/resume).
func (d *SegmentedDownloader) Snapshot() (int64, []Segment) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.single {
		return d.singleRecv.Load(), nil
	}
	var total int64
	segs := make([]Segment, len(d.segments))
	copy(segs, d.segments)
	for _, s := range segs {
		total += s.Received
	}
	return total, segs
}

func (d *SegmentedDownloader) finish(err error) {
	d.closeFile()
	d.finishOnce.Do(func() {
		if d.cfg.OnFinish != nil {
			d.cfg.OnFinish(err)
		}
	})
	d.cancel()
}

func (d *SegmentedDownloader) run() {
	canResume := d.cfg.ResumeTotal > 0 && len(d.cfg.ResumeSegs) > 0 && fileExists(d.cfg.PartPath)
	if canResume {
		d.mu.Lock()
		d.segments = append([]Segment(nil), d.cfg.ResumeSegs...)
		d.mu.Unlock()
		if err := d.openFile(false, 0); err != nil {
			d.finish(err)
			return
		}
		d.runMulti()
		return
	}
	var probe ProbeResult
	var err error
	for attempt := 1; ; attempt++ {
		probe, err = d.probe()
		var hs HTTPStatusError
		retryable := err != nil && d.ctx.Err() == nil &&
			((errors.As(err, &hs) && isThrottleStatus(hs.Status)) || !errors.As(err, &hs))
		if !retryable || attempt >= 4 {
			break
		}
		select { // brief backoff: 1.5 s, 3 s, 4.5 s
		case <-time.After(time.Duration(attempt) * 1500 * time.Millisecond):
		case <-d.ctx.Done():
		}
	}
	if err != nil {
		if d.ctx.Err() == nil {
			d.finish(err)
		}
		return
	}
	if d.cfg.OnProbe != nil {
		d.cfg.OnProbe(probe)
	}
	count := 1
	if probe.Ranges {
		if d.cfg.Auto {
			count = autoSegmentCount(probe.Total, d.cfg.SegmentCount)
		} else {
			count = int(probe.Total / d.cfg.MinSegment)
			if count > d.cfg.SegmentCount {
				count = d.cfg.SegmentCount
			}
			if count < 1 {
				count = 1
			}
		}
	}
	if probe.Ranges && count > 1 {
		d.mu.Lock()
		d.segments = makeSegments(probe.Total, count)
		d.mu.Unlock()
		if err := d.openFile(true, probe.Total); err != nil {
			d.finish(err)
			return
		}
		d.runMulti()
	} else {
		d.runSingle()
	}
}

// autoSegmentCount cuts a file into many small chunks (a work queue) so the number of live connections can change
// freely while downloading; tiny files use one stream because extra requests would cost more than they save.
func autoSegmentCount(total int64, maxConns int) int {
	const mib = 1 << 20
	if total < 2*mib {
		return 1
	}
	chunk := min(max(total/int64(maxConns*4), mib), 32*mib)
	return int(min(max((total+chunk-1)/chunk, 2), 128))
}

func (d *SegmentedDownloader) receivedTotal() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int64
	for _, s := range d.segments {
		n += s.Received
	}
	return n
}

// Conns returns the live connection count and the current target.
func (d *SegmentedDownloader) Conns() (int, int) { return int(d.conns.Load()), int(d.target.Load()) }

func makeSegments(total int64, count int) []Segment {
	size := total / int64(count)
	out := make([]Segment, count)
	for i := 0; i < count; i++ {
		start := int64(i) * size
		end := start + size - 1
		if i == count-1 {
			end = total - 1
		}
		out[i] = Segment{Start: start, End: end}
	}
	return out
}

func (d *SegmentedDownloader) newRequest(ctx context.Context, rng string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.requestURL.String(), nil)
	if err != nil {
		return nil, err
	}
	allowCreds := sameOrigin(d.requestURL, d.cfg.URL)
	for k, v := range d.cfg.Headers {
		if !allowCreds && isCredentialHeader(k) {
			continue
		}
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept-Encoding", "identity")
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	return req, nil
}

func isCredentialHeader(k string) bool {
	for _, h := range credentialHeaders {
		if strings.EqualFold(h, k) {
			return true
		}
	}
	return false
}

func (d *SegmentedDownloader) probe() (ProbeResult, error) {
	req, err := d.newRequest(d.ctx, "bytes=0-0")
	if err != nil {
		return ProbeResult{}, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return ProbeResult{}, err
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ProbeResult{}, HTTPStatusError{resp.StatusCode}
	}
	var p ProbeResult
	p.Total = -1
	if resp.StatusCode == 206 {
		cr := resp.Header.Get("Content-Range")
		if i := strings.LastIndex(cr, "/"); i >= 0 {
			if t, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64); err == nil {
				p.Total, p.Ranges = t, true
			}
		}
	} else {
		if resp.ContentLength > 0 {
			p.Total = resp.ContentLength
		}
		p.Ranges = p.Total > 0 && strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes")
	}
	if resp.Request != nil && resp.Request.URL != nil {
		d.requestURL = resp.Request.URL
		p.FinalURL = resp.Request.URL
	}
	p.SuggestedName = fileNameFromContentDisposition(resp.Header.Get("Content-Disposition"))
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		p.ContentType = strings.TrimSpace(strings.Split(ct, ";")[0])
	}
	return p, nil
}

func (d *SegmentedDownloader) openFile(create bool, prealloc int64) error {
	if err := os.MkdirAll(filepath.Dir(d.cfg.PartPath), 0o755); err != nil {
		return err
	}
	if create {
		// Remove instead of O_TRUNC: CREATE_ALWAYS fails on files with the hidden attribute.
		_ = os.Remove(d.cfg.PartPath)
	}
	f, err := os.OpenFile(d.cfg.PartPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("could not create file: %w", err)
	}
	hidePartFile(d.cfg.PartPath)
	if prealloc > 0 {
		// Sparse first: on NTFS, writing far past the valid data length would otherwise
		// force Windows to zero-fill gigabytes synchronously (disk stall).
		setSparse(f)
		if err := f.Truncate(prealloc); err != nil {
			f.Close()
			return err
		}
	}
	d.mu.Lock()
	d.file = f
	d.mu.Unlock()
	return nil
}

func (d *SegmentedDownloader) closeFile() {
	d.mu.Lock()
	if d.file != nil {
		_ = d.file.Close()
		d.file = nil
	}
	d.mu.Unlock()
}

type segResult struct {
	idx    int
	status int // HTTP status if the response was rejected, 0 otherwise
	err    error
}

var errRangeIgnored = errors.New("server ignored Range")

func (d *SegmentedDownloader) runMulti() {
	d.mu.Lock()
	var pending []int
	for i, s := range d.segments {
		if !s.Done() {
			pending = append(pending, i)
		}
	}
	d.mu.Unlock()
	if len(pending) == 0 {
		d.finish(nil)
		return
	}
	segCtx, segCancel := context.WithCancel(d.ctx)
	defer segCancel()
	results := make(chan segResult, d.cfg.SegmentCount+1)
	limit := d.cfg.SegmentCount
	ceil := d.cfg.SegmentCount // auto mode: never above what the server tolerated
	var tick <-chan time.Time
	var lastRecv, base int64
	var lastT, holdUntil time.Time
	var dir int
	if d.cfg.Auto {
		limit = min(2, ceil)
		if len(d.cfg.ResumeSegs) > 0 {
			limit = min(4, ceil)
		}
		tk := time.NewTicker(2 * time.Second)
		defer tk.Stop()
		tick, lastRecv, lastT = tk.C, d.receivedTotal(), time.Now()
	}
	active := 0
	retries := map[int]int{}
	var throttledUntil time.Time
	var wake *time.Timer

	for {
		now := time.Now()
		if now.After(throttledUntil) {
			for active < limit && len(pending) > 0 {
				i := pending[0]
				pending = pending[1:]
				active++
				go func(i int) { results <- d.downloadSegment(segCtx, i) }(i)
			}
		}
		d.conns.Store(int32(active))
		d.target.Store(int32(limit))
		if active == 0 && len(pending) == 0 {
			if d.allDone() {
				d.finish(nil)
			} else {
				d.finish(errors.New("download incomplete"))
			}
			return
		}
		var wakeC <-chan time.Time
		if active < limit && len(pending) > 0 && now.Before(throttledUntil) {
			wake = time.NewTimer(time.Until(throttledUntil) + 50*time.Millisecond)
			wakeC = wake.C
		}
		select {
		case <-d.ctx.Done():
			segCancel()
			for active > 0 {
				<-results
				active--
			}
			d.closeFile()
			return
		case <-wakeC:
			continue
		case now := <-tick:
			// Hill climbing: add a connection while the aggregate speed keeps rising by >6%, step back and hold
			// when it does not. A per-connection-throttled server climbs to the ceiling; a fast one stays at 2-3.
			recv := d.receivedTotal()
			sp := float64(recv-lastRecv) / now.Sub(lastT).Seconds()
			lastRecv, lastT = recv, now
			if now.Before(holdUntil) || len(pending) == 0 || active < limit {
				continue
			}
			if dir > 0 && sp <= float64(base)*1.06 {
				limit, dir, holdUntil = max(1, limit-1), 0, now.Add(15*time.Second)
			} else if limit < ceil {
				limit, dir, base = limit+1, 1, int64(sp)
			} else {
				dir = 0
			}
		case r := <-results:
			if wake != nil {
				wake.Stop()
				wake = nil
			}
			active--
			if d.ctx.Err() != nil {
				continue
			}
			switch {
			case errors.Is(r.err, errRangeIgnored):
				segCancel()
				for active > 0 {
					<-results
					active--
				}
				d.closeFile()
				d.runSingle()
				return
			case r.status != 0 && isThrottleStatus(r.status):
				// Server limiting: fewer parallel connections, back off, put the segment back first.
				limit = max(1, min(limit, active)-1)
				retries[r.idx]++
				if retries[r.idx] > d.cfg.MaxAttempts {
					d.abortAll(segCancel, results, &active, HTTPStatusError{r.status})
					return
				}
				throttledUntil = time.Now().Add(time.Duration(min(15, 1.5*float64(retries[r.idx])) * float64(time.Second)))
				pending = append([]int{r.idx}, pending...)
			case r.status != 0:
				d.abortAll(segCancel, results, &active, HTTPStatusError{r.status})
				return
			case r.err == nil && d.segmentDone(r.idx):
				retries[r.idx] = 0
				limit = min(d.cfg.SegmentCount, limit+1)
			default:
				var fe *fileError
				if errors.As(r.err, &fe) {
					d.abortAll(segCancel, results, &active, fe.err)
					return
				}
				retries[r.idx]++
				if retries[r.idx] > d.cfg.MaxAttempts {
					err := r.err
					if err == nil {
						err = fmt.Errorf("segment %d could not be completed", r.idx+1)
					}
					d.abortAll(segCancel, results, &active, err)
					return
				}
				t := time.Now().Add(time.Duration(min(10, retries[r.idx])) * time.Second)
				if t.After(throttledUntil) {
					throttledUntil = t
				}
				pending = append(pending, r.idx)
			}
		}
	}
}

func (d *SegmentedDownloader) abortAll(cancel context.CancelFunc, results chan segResult, active *int, err error) {
	cancel()
	for *active > 0 {
		<-results
		*active--
	}
	d.finish(err)
}

func isThrottleStatus(s int) bool {
	switch s {
	case 408, 425, 429, 500, 502, 503, 504:
		return true
	}
	return false
}

func (d *SegmentedDownloader) allDone() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.segments) == 0 {
		return false
	}
	for _, s := range d.segments {
		if !s.Done() {
			return false
		}
	}
	return true
}

func (d *SegmentedDownloader) segmentDone(i int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.segments[i].Done()
}

type fileError struct{ err error }

func (e *fileError) Error() string { return e.err.Error() }

// idleReader cancels the request if no data arrives for the idle timeout.
func withIdleTimeout(ctx context.Context, idle time.Duration) (context.Context, func(), func()) {
	c, cancel := context.WithCancel(ctx)
	t := time.AfterFunc(idle, cancel)
	return c, func() { t.Reset(idle) }, func() { t.Stop(); cancel() }
}

func (d *SegmentedDownloader) downloadSegment(ctx context.Context, i int) segResult {
	d.mu.Lock()
	seg := d.segments[i]
	d.mu.Unlock()
	if seg.Done() {
		return segResult{idx: i}
	}
	rctx, touch, done := withIdleTimeout(ctx, d.cfg.IdleTimeout)
	defer done()
	req, err := d.newRequest(rctx, fmt.Sprintf("bytes=%d-%d", seg.Start+seg.Received, seg.End))
	if err != nil {
		return segResult{idx: i, err: err}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return segResult{idx: i, err: err}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 206:
	case resp.StatusCode == 200:
		return segResult{idx: i, err: errRangeIgnored}
	default:
		return segResult{idx: i, status: resp.StatusCode}
	}
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			touch()
			if err := limiter.Wait(rctx, n); err != nil {
				return segResult{idx: i, err: err}
			}
			d.mu.Lock()
			s := d.segments[i]
			remaining := s.Length() - s.Received
			chunk := buf[:n]
			if int64(n) > remaining {
				chunk = buf[:remaining]
			}
			f := d.file
			if f == nil {
				d.mu.Unlock()
				return segResult{idx: i, err: context.Canceled}
			}
			off := s.Start + s.Received
			d.mu.Unlock()
			if len(chunk) > 0 {
				if _, err := f.WriteAt(chunk, off); err != nil {
					return segResult{idx: i, err: &fileError{err}}
				}
				d.mu.Lock()
				d.segments[i].Received += int64(len(chunk))
				doneNow := d.segments[i].Done()
				d.mu.Unlock()
				if doneNow {
					return segResult{idx: i}
				}
			} else {
				return segResult{idx: i}
			}
		}
		if rerr == io.EOF {
			return segResult{idx: i} // segmentDone() decides whether to retry
		}
		if rerr != nil {
			return segResult{idx: i, err: rerr}
		}
	}
}

func (d *SegmentedDownloader) runSingle() {
	d.mu.Lock()
	d.single = true
	d.segments = nil
	d.mu.Unlock()
	d.singleRecv.Store(0)
	if err := d.openFile(true, 0); err != nil {
		d.finish(err)
		return
	}
	rctx, touch, done := withIdleTimeout(d.ctx, d.cfg.IdleTimeout)
	defer done()
	req, err := d.newRequest(rctx, "")
	if err != nil {
		d.finish(err)
		return
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		if d.ctx.Err() == nil {
			d.finish(err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		d.finish(HTTPStatusError{resp.StatusCode})
		return
	}
	total := resp.ContentLength
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	d.mu.Lock()
	f := d.file
	d.mu.Unlock()
	var off int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			touch()
			if err := limiter.Wait(rctx, n); err != nil {
				break
			}
			if _, err := f.WriteAt(buf[:n], off); err != nil {
				d.finish(err)
				return
			}
			off += int64(n)
			d.singleRecv.Store(off)
		}
		if rerr == io.EOF {
			if total > 0 && off < total {
				d.finish(fmt.Errorf("connection closed early (%d/%d bytes)", off, total))
				return
			}
			d.finish(nil)
			return
		}
		if rerr != nil {
			break
		}
	}
	if d.ctx.Err() != nil {
		d.closeFile()
		return
	}
	d.finish(errors.New("connection lost"))
}

// ---------------------------------------------------------------------------------------------

var (
	reCD5987  = regexp.MustCompile(`(?i)filename\*\s*=\s*[^']*'[^']*'([^;]+)`)
	reCDQuote = regexp.MustCompile(`(?i)filename\s*=\s*"([^"]*)"`)
	reCDPlain = regexp.MustCompile(`(?i)filename\s*=\s*([^;]+)`)
)

func fileNameFromContentDisposition(h string) string {
	if h == "" {
		return ""
	}
	if _, params, err := mime.ParseMediaType(h); err == nil {
		if n := params["filename"]; n != "" {
			return n
		}
	}
	if m := reCD5987.FindStringSubmatch(h); m != nil {
		if v, err := url.PathUnescape(strings.TrimSpace(m[1])); err == nil && v != "" {
			return strings.Trim(v, `"`)
		}
	}
	if m := reCDQuote.FindStringSubmatch(h); m != nil && m[1] != "" {
		return m[1]
	}
	if m := reCDPlain.FindStringSubmatch(h); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
