# HRIS Auto Clock

Auto clock-in / clock-out harian ke HRIS Lumoshive, dengan UI web untuk mengatur
jam, lokasi, dan foto per hari (Senin–Jumat). Satu binary Go, tanpa dependency.

## Menjalankan

```bash
go build -o hris-autoclock .
./hris-autoclock
```

Lalu buka <http://127.0.0.1:8787>.

| Env | Default | Keterangan |
|---|---|---|
| `PORT` | `8787` | Port web UI |
| `HOST` | `127.0.0.1` | Alamat bind. **Jangan** diubah ke `0.0.0.0` kecuali paham risikonya |
| `DATA_DIR` | folder kerja | Lokasi `config.json`, `lastrun.json`, dan `images/` |

`DATA_DIR` penting kalau dijalankan sebagai service, karena folder kerja service
sering bukan folder aplikasi.

## Cara kerja

- **Jadwal** — scheduler mengecek tiap 20 detik. Absen dikirim saat jam terjadwal
  hari itu sudah lewat dan hari tersebut aktif.
- **Absen susulan** — kalau laptop tidur atau app baru dinyalakan setelah jam
  absen, absen tetap dikirim selama belum lewat *toleransi telat*
  (default 240 menit, bisa diubah di UI).
- **Percobaan ulang** — absen yang gagal (jaringan putus, server 500) dicoba lagi
  dengan jeda 1, 2, 4, 8, lalu 15 menit, maksimal 8 kali per slot per hari.
  Hanya absen yang benar-benar diterima server yang ditandai selesai.
- **Anti dobel** — satu slot (tanggal + clock-in/clock-out) hanya dikirim sekali.
  Riwayatnya disimpan di `lastrun.json` dan bertahan walau app di-restart.
  Respons server yang berarti "sudah absen" juga dihitung selesai.
- **Token** — kalau HRIS menolak token (401), app login ulang otomatis memakai
  email/password tersimpan lalu mengirim ulang absen tersebut.
- **Foto wajib** — HRIS menolak absen tanpa foto (`The photo field is required`).
  Slot yang belum punya foto tidak dikirim sama sekali dan tidak menghabiskan
  jatah percobaan; begitu fotonya diunggah, pengiriman jalan di tick berikutnya.

## Payload yang dikirim

`POST /api/attendance/clock-in` (atau `clock-out`), `multipart/form-data`,
header `Authorization: Bearer <token>`:

| Field | Contoh | Catatan |
|---|---|---|
| `latitude` | `-6.5024163` | dari pengaturan hari itu |
| `longitude` | `106.8028025` | |
| `radius` | `37933` | dihitung otomatis dari jarak ke kantor |
| `status` | `WFH` | `WFH` atau `WFO` |
| `date` | `2026-09-16` | format `Y-m-d` |
| `clock_in` / `clock_out` | `09:00` | format `H:i`, **jam aktual saat request dikirim** |
| `timezone` | `Asia/Jakarta` | |
| `photo` | file | wajib, dikirim dengan Content-Type asli (mis. `image/jpeg`) |

Nama field ini dipastikan dari tiga sumber: pesan validasi 422 HRIS asli, string
di dalam APK `com.lumoshive.hris` v1.0.3, dan bentuk resource yang dikembalikan
server di respons `/api/auth/login`.

## File data

- `config.json` — email, password, token, dan pengaturan per hari.
  Ditulis dengan permission `0600` karena berisi password teks biasa.
- `lastrun.json` — riwayat absen; entri lebih dari 14 hari dibersihkan sendiri.
- `images/day-{0-6}-{in|out}.{jpg|png|webp|gif|heic}` — foto per hari per slot.
  `0` = Minggu, `1` = Senin, dst. **Wajib ada untuk tiap slot yang aktif.**
  Kelola lewat UI, jangan taruh manual.

## Keamanan

- Server hanya mendengar di `127.0.0.1`. Kalau dibuka ke jaringan, siapa pun di
  jaringan itu bisa memicu absen atas nama Anda.
- `GET /api/config` tidak pernah mengirim password atau token, hanya penandanya.
- Request yang mengubah data ditolak kalau datang dari situs lain (proteksi CSRF),
  sehingga halaman web mana pun yang Anda buka tidak bisa menyuruh app ini absen.

## Endpoint

| Method | Path | Fungsi |
|---|---|---|
| `GET` | `/` | UI |
| `GET` `POST` | `/api/config` | Baca / simpan pengaturan |
| `POST` | `/api/test-login` | Uji login, simpan token |
| `GET` | `/api/status` | Jam server, status token, keadaan absen hari ini |
| `GET` | `/api/logs` | 200 log terakhir |
| `GET` | `/api/images` | Daftar foto |
| `POST` | `/api/upload` | Unggah foto (maks 10 MB, harus gambar asli) |
| `DELETE` | `/api/upload/{day}/{slot}` | Hapus foto |

## Jalan otomatis saat login (macOS)

Simpan sebagai `~/Library/LaunchAgents/com.ilham.hris-autoclock.plist`, ganti
`/PATH/KE` dengan lokasi folder aplikasi, lalu
`launchctl load ~/Library/LaunchAgents/com.ilham.hris-autoclock.plist`.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.ilham.hris-autoclock</string>
  <key>ProgramArguments</key>
  <array><string>/PATH/KE/hris-autoclock</string></array>
  <key>EnvironmentVariables</key>
  <dict><key>DATA_DIR</key><string>/PATH/KE</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/PATH/KE/hris-autoclock.log</string>
  <key>StandardErrorPath</key><string>/PATH/KE/hris-autoclock.log</string>
</dict>
</plist>
```

## Test

```bash
go test -race ./...
```
