# MC Download Manager — Windows

Windows için hafif indirme yöneticisi. [mstcvk/mc-download-manager](https://github.com/mstcvk/mc-download-manager) (macOS) projesinden esinlenen Go ile yazılmış bir port. Tek `.exe`, tepside çalışır, boştayken ~0 CPU ve ~8 MB bellek kullanır.

*A lightweight Windows port of the macOS MC Download Manager: one `.exe`, tray icon, browser extension (Chrome / Edge / Brave).*

## Özellikler
- Parçalı (çok bağlantılı) indirme, duraklat/sürdür, hız sınırı
- Chrome / Edge / Brave eklentisi: tarayıcı indirmelerini yakalar, videoların köşesinde indirme butonu gösterir
- yt-dlp ile video indirme: YouTube, X, Instagram, TikTok, Reddit, Facebook, Twitch, Kick…
- Canlı yayın kaydı (YouTube/Twitch'te baştan kayıt), başlangıç-bitiş aralığı indirme
- yt-dlp / ffmpeg / deno'yu arayüzden tek tıkla kurma, yt-dlp otomatik güncelleme
- Site bazında "bu sitede kullanma" anahtarı
- Pano izleme, Windows ile başlatma, arka planda düşük öncelik

## Kurulum
1. [Releases](../../releases) sayfasından `MCDownloadManager.exe` dosyasını indirip çalıştırın (tepside simge çıkar).
2. Arayüzde **Eklenti** sekmesine girip kullandığınız tarayıcının düğmesine basın (**Chrome'da kur / Edge'de kur / Brave'de kur**). Klasör yolu panoya kopyalanır ve tarayıcı eklentiler sayfasında açılır.
3. Açılan sayfada **Geliştirici modu**nu açın → **Paketlenmemiş öğe yükle** → klasör penceresinin adres çubuğuna `Ctrl+V` yapıp Enter'a basın (yol: `%APPDATA%\MC Download Manager\extension`) → **Klasör Seç**.
4. Video araçları için **Araçlar** sekmesinden yt-dlp ve ffmpeg'i kurun.

> Exe imzasız olduğu için SmartScreen "Yine de çalıştır" uyarısı verebilir.

## Kaynaktan derleme
Go 1.24+ gerekir, dış bağımlılık yoktur:

```
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui" -o MCDownloadManager.exe .
```

Yeni sürüm yayınlamak için `v1.2.6` gibi bir etiket gönderin; GitHub Actions exe'yi derleyip Release'e ekler.

## Güvenlik
Köprü yalnızca `127.0.0.1` üzerinde dinler ve sadece tarayıcı eklentilerinden gelen isteklere yanıt verir. Arayüz rastgele bir anahtarla korunur. İndirilen dosyalara "internetten indirildi" işareti eklenir.

## Lisans
MIT — bkz. [LICENSE](LICENSE). Orijinal proje © Mesut Çevik.
