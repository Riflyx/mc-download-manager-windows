package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const repoSlug = "Riflyx/mc-download-manager-windows"

type UpdateInfo struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

var (
	updateInfo     atomic.Pointer[UpdateInfo]
	updateNotified atomic.Bool
)

// versionNewer reports whether tag ("v1.3.1") is a higher version than cur ("1.3.0-win").
func versionNewer(tag, cur string) bool {
	parse := func(v string) [3]int {
		v = strings.TrimPrefix(strings.TrimSpace(v), "v")
		if i := strings.IndexAny(v, "-+ "); i >= 0 {
			v = v[:i]
		}
		var out [3]int
		for i, p := range strings.Split(v, ".") {
			if i == 3 {
				break
			}
			out[i], _ = strconv.Atoi(p)
		}
		return out
	}
	a, b := parse(tag), parse(cur)
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// scheduleUpdateCheck looks at the latest GitHub release twice a day (one tiny request; nothing runs when disabled).
func scheduleUpdateCheck() {
	go func() {
		time.Sleep(90 * time.Second)
		for {
			if appSettings.Get().CheckUpdates {
				checkUpdate()
			}
			time.Sleep(12 * time.Hour)
		}
	}()
}

func checkUpdate() {
	req, err := http.NewRequest("GET", "https://api.github.com/repos/"+repoSlug+"/releases/latest", nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", appName+"/"+appVersion)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var rel struct {
		Tag string `json:"tag_name"`
		URL string `json:"html_url"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&rel) != nil || rel.Tag == "" {
		return
	}
	if !versionNewer(rel.Tag, appVersion) || !strings.HasPrefix(rel.URL, "https://github.com/") {
		return
	}
	updateInfo.Store(&UpdateInfo{Version: strings.TrimPrefix(rel.Tag, "v"), URL: rel.URL})
	if updateNotified.CompareAndSwap(false, true) {
		notify(appName, Tf("updateAvail", strings.TrimPrefix(rel.Tag, "v")), rel.URL)
	}
}
