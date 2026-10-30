package main

import (
	"testing"
	"time"
)

func TestGuardStep(t *testing.T) {
	mb := float64(1 << 20)
	if r := guardStep(0, 10*mb, 40, false); r != int64(7*mb) {
		t.Fatalf("congestion from open: want 70%% of speed, got %d", r)
	}
	if r := guardStep(int64(3*mb), 3*mb, 40, false); r != int64(2.1*mb) {
		t.Fatalf("congestion with limit: got %d", r)
	}
	if r := guardStep(int64(1*mb), 5*mb, 40, false); r != int64(1*mb) {
		t.Fatalf("must never loosen while congested: got %d", r)
	}
	if r := guardStep(0, 100, 40, false); r != 256<<10 {
		t.Fatalf("floor not applied: %d", r)
	}
	if r := guardStep(int64(2*mb), 2*mb, 3, false); r <= int64(2*mb) {
		t.Fatalf("clean line must creep up: %d", r)
	}
	if r := guardStep(int64(10*mb), 1*mb, 3, false); r != 0 {
		t.Fatalf("limit that no longer binds must be dropped: %d", r)
	}
	if r := guardStep(int64(2*mb), 2*mb, 18, false); r != int64(2*mb) {
		t.Fatalf("inside the hysteresis band the rate must hold: %d", r)
	}
	if r := guardStep(0, 5*mb, 18, true); r == 0 {
		t.Fatalf("game mode margin is tighter (12ms): 18ms excess must cut")
	}
}

func TestGameCapAndSegments(t *testing.T) {
	if gameCap(0) != 0 || gameCap(100<<20) != 30<<20 || gameCap(1<<20) != 512<<10 {
		t.Fatal("gameCap")
	}
	if autoSegmentCount(1<<20, 16) != 1 || autoSegmentCount(50<<20, 16) != 50 || autoSegmentCount(100<<30, 16) != 128 {
		t.Fatalf("autoSegmentCount: %d %d %d", autoSegmentCount(1<<20, 16), autoSegmentCount(50<<20, 16), autoSegmentCount(100<<30, 16))
	}
}

func TestBaselineSamples(t *testing.T) {
	g := &netGuard{}
	g.addSample(0) // timeout before anything is known: ignored
	if g.base != 0 {
		t.Fatal("timeout must not set baseline")
	}
	g.addSample(20 * time.Millisecond)
	g.addSample(35 * time.Millisecond)
	g.addSample(15 * time.Millisecond)
	if g.base != 15 || g.rtt < 15 || g.rtt > 35 {
		t.Fatalf("base %.1f rtt %.1f", g.base, g.rtt)
	}
	g.addSample(0) // timeout once we know normal: counts as heavy congestion
	if g.rtt < 300 {
		t.Fatalf("timeout should push the smoothed RTT up, got %.1f", g.rtt)
	}
}

func TestVersionNewer(t *testing.T) {
	cases := []struct {
		tag, cur string
		want     bool
	}{
		{"v1.3.1", "1.3.0-win", true}, {"v1.3.0", "1.3.0-win", false}, {"v1.2.9", "1.3.0-win", false},
		{"v2.0.0", "1.9.9-win", true}, {"v1.10.0", "1.9.0-win", true}, {"garbage", "1.0.0", false},
	}
	for _, c := range cases {
		if got := versionNewer(c.tag, c.cur); got != c.want {
			t.Errorf("versionNewer(%q,%q)=%v want %v", c.tag, c.cur, got, c.want)
		}
	}
}
