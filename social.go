package main

import (
	"context"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------------------------
// URL normalization for social sites (embed-fixer mirrors, mobile hosts, share parameters).

var mirrorHosts = map[string]string{
	"fxtwitter.com": "x.com", "vxtwitter.com": "x.com", "fixupx.com": "x.com", "fixvx.com": "x.com",
	"twittpr.com": "x.com", "twitter.com": "x.com", "mobile.twitter.com": "x.com", "mobile.x.com": "x.com",
	"m.youtube.com": "www.youtube.com", "youtube.com": "www.youtube.com",
	"ddinstagram.com": "www.instagram.com", "instagramez.com": "www.instagram.com", "instagram.com": "www.instagram.com",
	"vxtiktok.com": "www.tiktok.com", "tnktok.com": "www.tiktok.com", "m.tiktok.com": "www.tiktok.com",
	"rxddit.com": "www.reddit.com", "vxreddit.com": "www.reddit.com", "old.reddit.com": "www.reddit.com",
	"m.facebook.com": "www.facebook.com", "mbasic.facebook.com": "www.facebook.com", "web.facebook.com": "www.facebook.com",
	"m.twitch.tv": "www.twitch.tv",
}

var dropParams = []string{"igsh", "igshid", "utm_source", "utm_medium", "utm_campaign", "utm_content", "utm_term",
	"s", "t_ref", "is_from_webapp", "sender_device", "share_id", "si", "mibextid", "rdid", "fbclid", "ref"}

func normalizeVideoURL(u *url.URL) *url.URL {
	c := *u
	h := strings.ToLower(c.Hostname())
	if to, ok := mirrorHosts[h]; ok {
		c.Host = to
		h = to
	}
	switch {
	case h == "www.instagram.com":
		c.Path = regexp.MustCompile(`^/reels/([^/]+)`).ReplaceAllString(c.Path, "/reel/$1")
	case h == "x.com":
		c.Path = regexp.MustCompile(`/(photo|video)/\d+/?$`).ReplaceAllString(c.Path, "")
	}
	q := c.Query()
	changed := false
	for _, p := range dropParams {
		if p == "s" && h != "x.com" { // "s" is only a share tracker on X
			continue
		}
		if p == "si" && !strings.Contains(h, "youtube") && h != "youtu.be" {
			continue
		}
		if q.Has(p) {
			q.Del(p)
			changed = true
		}
	}
	if changed {
		c.RawQuery = q.Encode()
	}
	return &c
}

// feedPageHint returns a message when the URL is a feed/profile/channel page rather than a single
// video. yt-dlp either rejects those ("Unsupported URL") or, worse, starts downloading a whole channel.
func feedPageHint(u *url.URL) string {
	h := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	p := strings.TrimSuffix(u.Path, "/")
	seg := strings.Split(strings.TrimPrefix(p, "/"), "/")
	first := ""
	if len(seg) > 0 {
		first = strings.ToLower(seg[0])
	}
	feed := false
	switch {
	case h == "x.com":
		feed = !strings.Contains(p, "/status/") && !strings.HasPrefix(p, "/i/broadcasts/") && !strings.HasPrefix(p, "/i/spaces/")
	case h == "instagram.com":
		feed = !(first == "p" || first == "reel" || first == "tv" || first == "stories") || len(seg) < 2
	case h == "tiktok.com":
		feed = !strings.Contains(p, "/video/") && !strings.Contains(p, "/photo/") && first != "t"
	case h == "vm.tiktok.com" || h == "vt.tiktok.com" || h == "youtu.be":
		feed = false
	case strings.HasSuffix(h, "youtube.com"):
		feed = !(first == "watch" || first == "shorts" || first == "live" || first == "clip" || first == "embed" || first == "v")
		if first == "playlist" {
			feed = true
		}
	case strings.HasSuffix(h, "reddit.com"):
		feed = !strings.Contains(p, "/comments/") && first != "video" && !(first == "r" && len(seg) >= 4 && seg[2] == "s")
	case strings.HasSuffix(h, "facebook.com"):
		feed = !(strings.Contains(p, "/videos/") || strings.Contains(p, "/reel/") || (first == "watch" && u.Query().Get("v") != "") ||
			first == "share" || first == "story.php" || strings.Contains(p, "/posts/"))
	case strings.HasSuffix(h, "twitch.tv"):
		feed = p == "" || first == "directory" || first == "search"
	case h == "kick.com":
		feed = p == "" || first == "categories" || first == "browse"
	}
	if feed {
		return T("errFeed")
	}
	return ""
}

// looksLikeVideoPost reports whether a copied link points at a single video post (clipboard prompt).
var reVideoPost = regexp.MustCompile(`(?i)^https?://(www\.|m\.|mobile\.|vm\.|vt\.)?(` +
	`(x|twitter|fxtwitter|vxtwitter|fixupx)\.com/[^/]+/status/\d+|` +
	`instagram\.com/(p|reels?|tv)/[^/]+|` +
	`tiktok\.com/(@[^/]+/video/\d+|t/\w+)|tiktok\.com/\w+/?$|` +
	`youtube\.com/(watch\?.*v=|shorts/|live/)|youtu\.be/|` +
	`reddit\.com/r/[^/]+/(comments|s)/|v\.redd\.it/|` +
	`twitch\.tv/([^/]+/clip/|videos/\d+)|clips\.twitch\.tv/|` +
	`kick\.com/[^/]+/(videos|clips)/|` +
	`facebook\.com/(.+/videos/|reel/|watch/?\?v=)|fb\.watch/|` +
	`vimeo\.com/\d+|dailymotion\.com/video/|bsky\.app/profile/[^/]+/post/|threads\.(net|com)/@[^/]+/post/)`)

func looksLikeVideoPost(u *url.URL) bool { return reVideoPost.MatchString(u.String()) }

// friendlyVideoError prefixes yt-dlp's error with what to do about it.
func friendlyVideoError(msg string, hadCookies bool) string {
	l := strings.ToLower(msg)
	hint := ""
	switch {
	case strings.Contains(l, "unable to connect") || strings.Contains(l, "timed out") || strings.Contains(l, "getaddrinfo") ||
		strings.Contains(l, "connection reset") || strings.Contains(l, "network is unreachable") || strings.Contains(l, "failed to resolve"):
		hint = T("errNetwork")
	case strings.Contains(l, "unsupported url"):
		hint = T("errFeed")
	case strings.Contains(l, "no video could be found") || strings.Contains(l, "there's no video in this") || strings.Contains(l, "no video formats found"):
		hint = T("errNoVideo")
	case strings.Contains(l, "http error 429") || strings.Contains(l, "rate-limit") || strings.Contains(l, "rate limit"):
		hint = T("errRate")
	case strings.Contains(l, "sign in to confirm") || strings.Contains(l, "login required") || strings.Contains(l, "log in") ||
		strings.Contains(l, "logged-in") || strings.Contains(l, "use --cookies") || strings.Contains(l, "authentication") ||
		strings.Contains(l, "private") || strings.Contains(l, "age-restricted") || strings.Contains(l, "nsfw"):
		if hadCookies {
			hint = T("errLoginCookies")
		} else {
			hint = T("errLogin")
		}
	case strings.Contains(l, "requested format is not available"):
		hint = T("errFormat")
	case strings.Contains(l, "unable to extract") || strings.Contains(l, "please report this issue"):
		hint = T("errOutdated")
	}
	if hint == "" {
		return msg
	}
	return hint + " — " + msg
}

// ---------------------------------------------------------------------------------------------
// Daily yt-dlp self-update (social sites change often; old yt-dlp builds break quickly).

func scheduleYtdlpAutoUpdate() {
	go func() {
		time.Sleep(2 * time.Minute) // never compete with startup
		for {
			st := appSettings.Get()
			if st.AutoUpdateYtdlp && time.Since(time.Unix(st.LastYtdlpUpdate, 0)) > 20*time.Hour {
				updateYtdlpNow()
			}
			time.Sleep(6 * time.Hour)
		}
	}()
}

func updateYtdlpNow() {
	p := locateYtDlp(appSettings.Get().YtdlpPath)
	// Only self-update the copy we manage; never touch a pip/winget/scoop install.
	if p == "" || !strings.EqualFold(filepath.Dir(p), binDir()) || appStore.activeVideoCount() > 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, p, "-U")
	cmd.SysProcAttr = childProcAttr()
	if err := cmd.Run(); err == nil {
		appSettings.Update(func(s *Settings) { s.LastYtdlpUpdate = time.Now().Unix() })
	}
}
