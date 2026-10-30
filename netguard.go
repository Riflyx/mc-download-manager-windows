package main

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// netGuard keeps a download from hurting latency-sensitive traffic (games, calls). While downloads run it
// probes the round-trip time to a public host once a second; queueing in front of the line (bufferbloat)
// shows up as extra RTT. When RTT rises above the idle baseline it cuts the global rate to a fraction of what
// is currently flowing, then creeps back up while the line stays clean (AIMD, like LEDBAT). A fullscreen
// app (a game) tightens the margin and caps the rate to a share of the learned line speed.
//
// It only runs while something downloads, so an idle app has no timers and makes no probes.

type netGuard struct {
	mu       sync.Mutex
	rtt      float64 // smoothed RTT, ms
	base     float64 // lowest RTT seen (idle baseline), ms; 0 = unknown
	rate     int64   // guard rate in bytes/s, 0 = open
	link     float64 // learned line speed in bytes/s
	game     bool
	active   bool
	failures int
	probing  atomic.Bool
}

var guard = &netGuard{}

type guardStatus struct {
	Active   bool    `json:"active"`
	RTT      float64 `json:"rtt"`
	Base     float64 `json:"base"`
	RateKBps int64   `json:"rateKBps"` // 0 = no limit
	Game     bool    `json:"game"`
	LinkKBps int64   `json:"linkKBps"`
}

func (g *netGuard) Status() guardStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	return guardStatus{g.active, g.rtt, g.base, g.rate >> 10, g.game, int64(g.link) >> 10}
}

func (g *netGuard) Rate() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rate
}

// guardStep is the control law: given how far the RTT sits above its baseline, return the next guard rate.
func guardStep(rate int64, speed, excessMs float64, game bool) int64 {
	margin, floor := 25.0, int64(256<<10)
	if game {
		margin, floor = 12.0, 128<<10
	}
	switch {
	case excessMs > margin: // line is queueing: back off to 70% of what is flowing, never loosen
		r := max(int64(speed*0.7), floor)
		if rate != 0 && r > rate {
			r = rate
		}
		return r
	case excessMs < margin/2 && rate != 0: // clean line: creep up, drop the limit once it stops binding
		if float64(rate) > 2*speed {
			return 0
		}
		return int64(float64(rate)*1.12) + 32<<10
	}
	return rate
}

// gameCap is the most a fullscreen app leaves to downloads: 30% of the learned line speed (0 = unknown).
func gameCap(link float64) int64 {
	if link <= 0 {
		return 0
	}
	return max(int64(link*0.3), 512<<10)
}

func (g *netGuard) addSample(rtt time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ms := float64(rtt) / float64(time.Millisecond)
	if rtt <= 0 { // timeout: only meaningful when we already know what normal looks like
		g.failures++
		if g.base == 0 || g.failures > 3 {
			return
		}
		ms = 1000
	} else {
		g.failures = 0
		ms = max(ms, 1)
	}
	if g.rtt == 0 {
		g.rtt = ms
	} else {
		g.rtt = 0.6*g.rtt + 0.4*ms
	}
	if g.base == 0 || ms < g.base {
		g.base = ms
	} else {
		g.base += (ms - g.base) * 0.005 // slow upward drift so a route change does not pin the baseline too low
	}
}

func probeRTT() time.Duration {
	for _, addr := range []string{"1.1.1.1:443", "8.8.8.8:443"} {
		t0 := time.Now()
		c, err := net.DialTimeout("tcp", addr, 800*time.Millisecond)
		if err == nil {
			d := time.Since(t0)
			_ = c.Close()
			return d
		}
	}
	return 0
}

// Tick is called about once a second while downloads run, with the aggregate file speed in bytes/s.
func (g *netGuard) Tick(speed float64) {
	st := appSettings.Get()
	if !st.GameFriendly {
		g.Idle()
		return
	}
	game := isFullscreenApp()
	if g.probing.CompareAndSwap(false, true) {
		go func() { g.addSample(probeRTT()); g.probing.Store(false) }()
	}
	g.mu.Lock()
	g.active, g.game = true, game
	if g.link == 0 && st.LinkKBps > 0 {
		g.link = float64(st.LinkKBps) * 1024
	}
	if g.base > 0 && g.rtt > 0 {
		g.rate = guardStep(g.rate, speed, g.rtt-g.base, game)
	}
	if game {
		if c := gameCap(g.link); c > 0 && (g.rate == 0 || g.rate > c) {
			g.rate = c
		}
	} else if g.rate == 0 && st.SpeedLimitBytes() == 0 {
		g.link = max(g.link*0.999, speed) // line speed estimate: only from unthrottled samples, slowly forgets
	}
	g.mu.Unlock()
	applyRate(st)
}

// Idle runs when nothing downloads (or the feature is off): release the limit and remember the line speed.
func (g *netGuard) Idle() {
	g.mu.Lock()
	g.active, g.rate, g.game = false, 0, false
	kb := int(g.link) >> 10
	g.mu.Unlock()
	if old := appSettings.Get().LinkKBps; kb > 0 && (kb > old*12/10 || kb < old*8/10) {
		appSettings.Update(func(s *Settings) { s.LinkKBps = kb })
	}
	applyRate(appSettings.Get())
}

// applyRate sets the global limiter: the stricter of the manual limit and the guard rate.
func applyRate(s Settings) {
	r := s.SpeedLimitBytes()
	if gr := guard.Rate(); gr > 0 && (r == 0 || gr < r) {
		r = gr
	}
	limiter.SetRate(r)
}
