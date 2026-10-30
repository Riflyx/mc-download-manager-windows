package main

import (
	"net/url"
	"testing"
)

func mu(s string) *url.URL { u, _ := url.Parse(s); return u }

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"https://fxtwitter.com/a/status/123":            "https://x.com/a/status/123",
		"https://twitter.com/a/status/123/video/1":      "https://x.com/a/status/123",
		"https://x.com/a/status/123?s=20":               "https://x.com/a/status/123",
		"https://www.instagram.com/reels/AbC/?igsh=xyz": "https://www.instagram.com/reel/AbC/",
		"https://m.youtube.com/watch?v=abc&si=zz":       "https://www.youtube.com/watch?v=abc",
		"https://vxtiktok.com/@u/video/1":               "https://www.tiktok.com/@u/video/1",
	}
	for in, want := range cases {
		if got := normalizeVideoURL(mu(in)).String(); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

func TestFeedHint(t *testing.T) {
	feed := []string{"https://x.com/home", "https://x.com/elonmusk", "https://www.instagram.com/", "https://www.instagram.com/someone/",
		"https://www.tiktok.com/foryou", "https://www.tiktok.com/@user", "https://www.youtube.com/", "https://www.youtube.com/@MrBeast",
		"https://www.youtube.com/playlist?list=x", "https://www.reddit.com/r/videos/", "https://www.facebook.com/"}
	single := []string{"https://x.com/a/status/1", "https://www.instagram.com/reel/AbC/", "https://www.instagram.com/p/AbC/",
		"https://www.tiktok.com/@u/video/123", "https://vm.tiktok.com/ZMabc/", "https://www.youtube.com/watch?v=a", "https://youtu.be/a",
		"https://www.youtube.com/shorts/a", "https://www.reddit.com/r/videos/comments/abc/t/", "https://www.twitch.tv/videos/123",
		"https://kick.com/xqc", "https://www.facebook.com/watch/?v=1", "https://example.com/page"}
	for _, s := range feed {
		if feedPageHint(normalizeVideoURL(mu(s))) == "" {
			t.Errorf("expected feed: %s", s)
		}
	}
	for _, s := range single {
		if feedPageHint(normalizeVideoURL(mu(s))) != "" {
			t.Errorf("expected single: %s", s)
		}
	}
}

func TestVideoPost(t *testing.T) {
	yes := []string{"https://x.com/a/status/1", "https://www.instagram.com/reel/x/", "https://youtu.be/abc", "https://www.tiktok.com/@u/video/1", "https://vm.tiktok.com/ZMabc/", "https://clips.twitch.tv/Abc"}
	no := []string{"https://x.com/home", "https://www.youtube.com/", "https://example.com/a.zip", "https://www.instagram.com/someone/"}
	for _, s := range yes {
		if !looksLikeVideoPost(mu(s)) {
			t.Errorf("want post: %s", s)
		}
	}
	for _, s := range no {
		if looksLikeVideoPost(mu(s)) {
			t.Errorf("not post: %s", s)
		}
	}
}

func TestFriendly(t *testing.T) {
	if got := friendlyVideoError("ERROR: Unsupported URL: https://x.com/home", false); got == "ERROR: Unsupported URL: https://x.com/home" {
		t.Error("no hint")
	}
}

func TestFriendlyNetwork(t *testing.T) {
	got := friendlyVideoError("ERROR: [TikTok] 1: Unable to download webpage: ('Unable to connect to proxy'); please report this issue", false)
	if got[:len(T("errNetwork"))] != T("errNetwork") {
		t.Error(got)
	}
}
