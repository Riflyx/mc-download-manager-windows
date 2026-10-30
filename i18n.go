package main

import "fmt"

var lang = "en" // set at startup from the Windows UI language

var strs = map[string][2]string{ // key -> {en, tr}
	"catimage":      {"Images", "Resimler"},
	"catmusic":      {"Music", "Müzik"},
	"catvideo":      {"Video", "Video"},
	"catapps":       {"Apps", "Uygulamalar"},
	"catdocument":   {"Documents", "Belgeler"},
	"catcompressed": {"Archives", "Arşivler"},
	"catother":      {"Other", "Diğer"},

	"stageAudio":       {"Downloading audio…", "Ses indiriliyor…"},
	"stageVideo":       {"Downloading video…", "Video indiriliyor…"},
	"stageDownloading": {"Downloading…", "İndiriliyor…"},
	"stageMerge":       {"Merging video and audio (ffmpeg)…", "Video ve ses birleştiriliyor (ffmpeg)…"},
	"stageRemux":       {"Converting container (ffmpeg)…", "Kapsayıcı dönüştürülüyor (ffmpeg)…"},
	"stageExtract":     {"Extracting audio (ffmpeg)…", "Ses çıkarılıyor (ffmpeg)…"},
	"stageSelected":    {"Selected: ", "Seçilen: "},
	"stageAlready":     {"File already downloaded", "Dosya zaten indirilmiş"},
	"stageInfo":        {"Fetching video information…", "Video bilgisi alınıyor…"},
	"stageDenoWait":    {"Waiting for the JavaScript runtime (deno)…", "JavaScript çalışma zamanı (deno) bekleniyor…"},
	"stageStopping":    {"Stopping recording, saving file…", "Kayıt durduruluyor, dosya kaydediliyor…"},
	"stageDenoInstall": {"Installing deno…", "deno kuruluyor…"},

	"errNoYtdlp":     {"yt-dlp not found. Install it from Settings ▸ Tools.", "yt-dlp bulunamadı. Ayarlar ▸ Araçlar'dan kurun."},
	"retryNoCookies": {"→ YouTube rejected the signed-in request; retrying without cookies…", "→ YouTube oturumlu isteği reddetti; çerezsiz yeniden deneniyor…"},

	"trayOpen":        {"Open MC Download Manager", "MC Download Manager'ı aç"},
	"trayPauseAll":    {"Pause all", "Tümünü duraklat"},
	"trayResumeAll":   {"Resume all", "Tümünü sürdür"},
	"trayFolder":      {"Open download folder", "İndirme klasörünü aç"},
	"traySpeed":       {"Speed limit (%s)", "Hız sınırı (%s)"},
	"traySpeedNone":   {"Speed limit (set in Settings)", "Hız sınırı (Ayarlar'dan belirle)"},
	"trayClipboard":   {"Watch clipboard", "Panoyu izle"},
	"trayAutostart":   {"Start with Windows", "Windows ile başlat"},
	"trayQuit":        {"Quit", "Çıkış"},
	"tipIdle":         {"MC Download Manager", "MC Download Manager"},
	"tipActive":       {"MC Download Manager — %d active, %s", "MC Download Manager — %d aktif, %s"},
	"notifDone":       {"Download completed", "İndirme tamamlandı"},
	"notifFailed":     {"Download failed", "İndirme başarısız"},
	"clipTitle":       {"MC Download Manager", "MC Download Manager"},
	"clipOne":         {"A link was copied:\n\n%s\n\nYes = download file\nNo = download as video (yt-dlp)\nCancel = ignore", "Bir bağlantı kopyalandı:\n\n%s\n\nEvet = dosya olarak indir\nHayır = video olarak indir (yt-dlp)\nİptal = yoksay"},
	"clipMany":        {"%d links were copied:\n\n%s\n\nYes = download files\nNo = download as video (yt-dlp)\nCancel = ignore", "%d bağlantı kopyalandı:\n\n%s\n\nEvet = dosya olarak indir\nHayır = video olarak indir (yt-dlp)\nİptal = yoksay"},
	"denoTitle":       {"Install the JavaScript runtime for YouTube?", "YouTube için JavaScript çalışma zamanı kurulsun mu?"},
	"denoBody":        {"YouTube now requires yt-dlp to run JavaScript (deno). Without it, YouTube downloads may fail.\n\nDownload deno (~40 MB) from its official GitHub releases now?\n\n(Cancel = don't ask again)", "YouTube artık yt-dlp'nin JavaScript (deno) çalıştırmasını gerektiriyor. Olmadan YouTube indirmeleri başarısız olabilir.\n\ndeno (~40 MB) resmi GitHub sürümlerinden şimdi indirilsin mi?\n\n(İptal = bir daha sorma)"},
	"errFeed":         {"This is a feed/profile page, not a single video. Use the “Download” button on the video, or copy the post's own link (Share → Copy link).", "Bu bir akış/profil sayfası, tek bir video değil. Videonun üzerindeki “İndir” butonunu kullanın ya da gönderinin kendi bağlantısını kopyalayın (Paylaş → Bağlantıyı kopyala)."},
	"errNoVideo":      {"This post has no video (maybe only images or a GIF link).", "Bu gönderide video yok (yalnızca resim olabilir)."},
	"errRate":         {"The site is rate-limiting; try again in a few minutes.", "Site geçici olarak sınırladı; birkaç dakika sonra tekrar deneyin."},
	"errLogin":        {"Sign-in required: download it from the browser (button on the video or right-click), so your login cookies are sent.", "Oturum gerekiyor: tarayıcıdan indirin (videonun üzerindeki buton ya da sağ tık) — böylece giriş çerezleriniz gönderilir."},
	"errLoginCookies": {"Sign-in required: make sure you are logged in to the site in this browser, then retry.", "Oturum gerekiyor: bu tarayıcıda siteye giriş yaptığınızdan emin olup tekrar deneyin."},
	"errFormat":       {"The selected quality is not available; set Settings ▸ Video ▸ Quality to “Best”.", "Seçilen kalite yok; Ayarlar ▸ Video ▸ Kalite'yi “En iyi” yapın."},
	"errNetwork":      {"Could not reach the site (connection problem, VPN/DNS or the site is down).", "Siteye ulaşılamadı (bağlantı sorunu, VPN/DNS ya da site çökmüş olabilir)."},
	"errOutdated":     {"The site changed; update yt-dlp in Tools and retry.", "Site değişmiş; Araçlar'dan yt-dlp'yi güncelleyip tekrar deneyin."},
	"clipVideo":       {"A video link was copied:\n\n%s\n\nDownload it with yt-dlp?", "Bir video bağlantısı kopyalandı:\n\n%s\n\nyt-dlp ile indirilsin mi?"},
	"alreadyRunning":  {"MC Download Manager is already running.", "MC Download Manager zaten çalışıyor."},
	"portBusy":        {"Port %d is in use. Change it in Settings.", "%d portu kullanımda. Ayarlar'dan değiştirin."},
}

func T(key string) string {
	v, ok := strs[key]
	if !ok {
		return key
	}
	if lang == "tr" {
		return v[1]
	}
	return v[0]
}

func Tf(key string, a ...any) string { return fmt.Sprintf(T(key), a...) }

func fmtSize(b int64) string {
	if b < 0 {
		return "?"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(b)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", b)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}
