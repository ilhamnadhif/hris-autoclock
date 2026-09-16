package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

const (
	defaultBaseURL = "https://hris.lumoshive.com"
	defaultHost    = "127.0.0.1"
	defaultPort    = "8787"
	defaultRadius  = 150

	// Absen yang terlewat (laptop tidur, app mati) masih dikejar selama
	// belum lewat jendela ini, dihitung dari jam terjadwal.
	defaultCatchupMinutes = 240
	maxCatchupMinutes     = 1440

	// Batas percobaan ulang bila server menolak absen (mis. HTTP 500).
	maxAttempts = 8

	maxLogs           = 200
	maxUploadBytes    = 10 << 20
	schedulerInterval = 20 * time.Second
	tokenCacheTTL     = 60 * time.Second
	lastRunRetention  = 14 * 24 * time.Hour

	clockIn  = "clock-in"
	clockOut = "clock-out"

	// Dikirim ke HRIS sebagai field "timezone" dan dipakai untuk time.Local.
	appTimezone = "Asia/Jakarta"
)

var dayNames = []string{"Minggu", "Senin", "Selasa", "Rabu", "Kamis", "Jumat", "Sabtu"}

var imageNameRe = regexp.MustCompile(`^day-[0-6]-(in|out)\.[a-zA-Z0-9]+$`)

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

// ---------------------------------------------------------------- konfigurasi

type ClockConfig struct {
	Time      string  `json:"time"`
	Latitude  string  `json:"latitude"`
	Longitude string  `json:"longitude"`
	Radius    float64 `json:"radius"`
	Status    string  `json:"status"`
}

type DayConfig struct {
	Enabled  bool        `json:"enabled"`
	ClockIn  ClockConfig `json:"clock_in"`
	ClockOut ClockConfig `json:"clock_out"`
}

type Config struct {
	Email          string               `json:"email"`
	Password       string               `json:"password"`
	Token          string               `json:"token"`
	BaseURL        string               `json:"base_url"`
	CatchupMinutes int                  `json:"catchup_minutes"`
	Days           map[string]DayConfig `json:"days"`
}

func defaultClock(hm string) ClockConfig {
	return ClockConfig{
		Time:      hm,
		Latitude:  "-6.1648863",
		Longitude: "106.7634657",
		Radius:    defaultRadius,
		Status:    "WFH",
	}
}

func defaultConfig() Config {
	days := map[string]DayConfig{}
	for i := 1; i <= 5; i++ {
		days[strconv.Itoa(i)] = DayConfig{
			Enabled:  true,
			ClockIn:  defaultClock("09:00"),
			ClockOut: defaultClock("18:00"),
		}
	}
	return Config{
		BaseURL:        defaultBaseURL,
		CatchupMinutes: defaultCatchupMinutes,
		Days:           days,
	}
}

func cloneConfig(c Config) Config {
	out := c
	out.Days = make(map[string]DayConfig, len(c.Days))
	for k, v := range c.Days {
		out.Days[k] = v
	}
	return out
}

func normalizeClock(cc ClockConfig, fallbackTime string) ClockConfig {
	cc.Latitude = strings.TrimSpace(cc.Latitude)
	cc.Longitude = strings.TrimSpace(cc.Longitude)
	if cc.Status == "" {
		cc.Status = "WFH"
	}
	if cc.Radius <= 0 {
		cc.Radius = defaultRadius
	}
	if _, _, ok := parseHM(cc.Time); !ok {
		cc.Time = fallbackTime
	}
	return cc
}

func normalizeConfig(cfg Config) Config {
	cfg.Email = strings.TrimSpace(cfg.Email)
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	cfg.CatchupMinutes = clampCatchup(cfg.CatchupMinutes)
	if len(cfg.Days) == 0 {
		cfg.Days = defaultConfig().Days
	}
	for k, d := range cfg.Days {
		d.ClockIn = normalizeClock(d.ClockIn, "09:00")
		d.ClockOut = normalizeClock(d.ClockOut, "18:00")
		cfg.Days[k] = d
	}
	return cfg
}

func clampCatchup(m int) int {
	if m <= 0 {
		return defaultCatchupMinutes
	}
	if m > maxCatchupMinutes {
		return maxCatchupMinutes
	}
	return m
}

func validateClock(cc ClockConfig) error {
	if _, _, ok := parseHM(cc.Time); !ok {
		return fmt.Errorf("jam %q tidak valid", cc.Time)
	}
	lat, err := strconv.ParseFloat(cc.Latitude, 64)
	if err != nil || lat < -90 || lat > 90 {
		return fmt.Errorf("latitude %q tidak valid", cc.Latitude)
	}
	lng, err := strconv.ParseFloat(cc.Longitude, 64)
	if err != nil || lng < -180 || lng > 180 {
		return fmt.Errorf("longitude %q tidak valid", cc.Longitude)
	}
	if cc.Radius <= 0 {
		return errors.New("radius harus lebih dari 0")
	}
	if strings.TrimSpace(cc.Status) == "" {
		return errors.New("status kosong")
	}
	return nil
}

// ------------------------------------------------------------------ riwayat

// RunState menyimpan hasil absen satu slot (tanggal + clock-in/clock-out).
// Done berarti tidak perlu dicoba lagi hari ini.
type RunState struct {
	Time        string `json:"time"`
	Done        bool   `json:"done"`
	Attempts    int    `json:"attempts"`
	LastAttempt string `json:"last_attempt,omitempty"`
	Message     string `json:"message,omitempty"`
}

// UnmarshalJSON menerima format lama ("09:00:13") maupun format baru (objek).
func (r *RunState) UnmarshalJSON(data []byte) error {
	var legacy string
	if err := json.Unmarshal(data, &legacy); err == nil {
		*r = RunState{Time: legacy, Done: true, Attempts: 1}
		return nil
	}
	type plain RunState
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*r = RunState(p)
	return nil
}

func (r RunState) lastAttemptTime() (time.Time, bool) {
	if r.LastAttempt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, r.LastAttempt)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// backoff menentukan jeda sebelum percobaan ulang: 1, 2, 4, 8, lalu 15 menit.
func backoff(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	if attempts > 5 {
		return 15 * time.Minute
	}
	d := time.Minute << uint(attempts-1)
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	return d
}

type LogEntry struct {
	Time    string `json:"time"`
	Action  string `json:"action"`
	Message string `json:"message"`
	Success bool   `json:"success"`
	Count   int    `json:"count"`
}

// ---------------------------------------------------------------------- app

type App struct {
	mu      sync.Mutex
	loginMu sync.Mutex

	config   Config
	logs     []LogEntry
	lastRun  map[string]RunState
	inFlight map[string]bool

	lastVerify     time.Time
	lastVerifyOK   bool
	verifyInFlight bool
	lastLoginError string

	configPath  string
	lastRunPath string
	imagesDir   string
	client      *http.Client
}

func newApp() *App {
	return &App{
		config:      defaultConfig(),
		lastRun:     map[string]RunState{},
		inFlight:    map[string]bool{},
		configPath:  "config.json",
		lastRunPath: "lastrun.json",
		imagesDir:   "images",
		client:      &http.Client{Timeout: 30 * time.Second},
	}
}

var app = newApp()

func (a *App) snapshot() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneConfig(a.config)
}

func (a *App) loadConfig() error {
	data, err := os.ReadFile(a.configPath)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("baca %s: %w", a.configPath, err)
		}
		a.config = defaultConfig()
		return a.saveConfigLocked()
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse %s: %w", a.configPath, err)
	}
	a.config = normalizeConfig(cfg)
	// Tulis ulang supaya field baru terisi dan permission-nya rapat sejak awal
	// (file ini menyimpan password dalam teks biasa).
	return a.saveConfigLocked()
}

func (a *App) saveConfigLocked() error {
	data, err := json.MarshalIndent(a.config, "", "  ")
	if err != nil {
		return err
	}
	// 0600: file ini menyimpan password & token dalam teks biasa.
	return writeFileAtomic(a.configPath, data, 0o600)
}

func (a *App) loadLastRun() error {
	data, err := os.ReadFile(a.lastRunPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("baca %s: %w", a.lastRunPath, err)
	}
	state := map[string]RunState{}
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse %s: %w", a.lastRunPath, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastRun = state
	before := len(state)
	a.pruneLastRunLocked(time.Now())
	if len(a.lastRun) != before {
		return a.saveLastRunLocked()
	}
	return nil
}

func (a *App) saveLastRunLocked() error {
	data, err := json.MarshalIndent(a.lastRun, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.lastRunPath, data, 0o644)
}

func (a *App) pruneLastRunLocked(now time.Time) {
	cutoff := now.Add(-lastRunRetention)
	for k := range a.lastRun {
		datePart, _, ok := strings.Cut(k, ":")
		if !ok {
			delete(a.lastRun, k)
			continue
		}
		d, err := time.ParseInLocation("2006-01-02", datePart, now.Location())
		if err != nil || d.Before(cutoff) {
			delete(a.lastRun, k)
		}
	}
}

// addLog menggabungkan pesan identik yang berurutan. Tanpa ini, satu
// konfigurasi yang rusak akan menulis log tiap 20 detik sampai log lain
// terdorong keluar.
func (a *App) addLog(action, msg string, success bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now().Format("2006-01-02 15:04:05")
	if n := len(a.logs); n > 0 {
		if last := &a.logs[n-1]; last.Action == action && last.Message == msg && last.Success == success {
			last.Time = now
			last.Count++
			return
		}
	}
	a.logs = append(a.logs, LogEntry{Time: now, Action: action, Message: msg, Success: success, Count: 1})
	if len(a.logs) > maxLogs {
		a.logs = append([]LogEntry(nil), a.logs[len(a.logs)-maxLogs:]...)
	}
}

// ------------------------------------------------------------------- login

func (a *App) loginErrorMsg(statusCode int, body []byte) string {
	var res struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &res) == nil && res.Message != "" {
		msg := strings.ToLower(res.Message)
		switch {
		case strings.Contains(msg, "credential") || strings.Contains(msg, "invalid"):
			return "email atau password salah"
		case strings.Contains(msg, "unauth"):
			return "tidak diizinkan (unauthorized)"
		case strings.Contains(msg, "not found") || strings.Contains(msg, "email"):
			return "email tidak terdaftar"
		case strings.Contains(msg, "blocked") || strings.Contains(msg, "suspend"):
			return "akun diblokir"
		}
		return oneLine(strings.ToLower(res.Message), 200)
	}
	return fmt.Sprintf("HTTP %d", statusCode)
}

func (a *App) login(email, password string) (string, error) {
	baseURL := a.snapshot().BaseURL
	payload, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/auth/login", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(a.loginErrorMsg(resp.StatusCode, body))
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("respons login tidak dikenali: %s", oneLine(string(body), 120))
	}
	if result.Token == "" {
		return "", errors.New("server tidak mengirim token")
	}
	return result.Token, nil
}

// refreshToken login ulang memakai kredensial tersimpan. Bila goroutine lain
// sudah lebih dulu memperbarui token, token baru itu yang dipakai.
func (a *App) refreshToken(stale string) (string, error) {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()

	a.mu.Lock()
	current := a.config.Token
	email, password := a.config.Email, a.config.Password
	a.mu.Unlock()

	if current != "" && current != stale {
		return current, nil
	}
	if email == "" || password == "" {
		return "", errors.New("email/password belum diisi")
	}

	token, err := a.login(email, password)
	if err != nil {
		a.mu.Lock()
		a.lastLoginError = err.Error()
		a.lastVerifyOK = false
		a.lastVerify = time.Now()
		a.mu.Unlock()
		return "", err
	}

	a.mu.Lock()
	a.config.Token = token
	a.lastLoginError = ""
	a.lastVerifyOK = true
	a.lastVerify = time.Now()
	saveErr := a.saveConfigLocked()
	a.mu.Unlock()

	if saveErr != nil {
		a.addLog("refresh-token", "Token baru gagal disimpan: "+saveErr.Error(), false)
	} else {
		a.addLog("refresh-token", "Token diperbarui otomatis", true)
	}
	return token, nil
}

func (a *App) verifyToken(token string) bool {
	baseURL := a.snapshot().BaseURL
	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/auth/user", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == http.StatusOK
}

// tokenStatus mengembalikan hasil verifikasi terakhir tanpa memblokir request.
// Bila cache sudah basi, verifikasi baru dijalankan di belakang layar.
func (a *App) tokenStatus() (valid bool, checked time.Time) {
	a.mu.Lock()
	token := a.config.Token
	valid, checked = a.lastVerifyOK, a.lastVerify
	if token != "" && !a.verifyInFlight && time.Since(checked) >= tokenCacheTTL {
		a.verifyInFlight = true
		go a.runVerify(token)
	}
	a.mu.Unlock()
	if token == "" {
		return false, time.Time{}
	}
	return valid, checked
}

func (a *App) runVerify(token string) {
	ok := a.verifyToken(token)
	a.mu.Lock()
	a.lastVerify = time.Now()
	a.lastVerifyOK = ok
	a.verifyInFlight = false
	a.mu.Unlock()
}

// ------------------------------------------------------------------- absen

func (a *App) photoFiles(day int, slot string) []string {
	entries, err := os.ReadDir(a.imagesDir)
	if err != nil {
		return nil
	}
	prefix := fmt.Sprintf("day-%d-%s.", day, slot)
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			out = append(out, filepath.Join(a.imagesDir, e.Name()))
		}
	}
	return out
}

// photoFor memilih foto terbaru bila (karena alasan apa pun) ada lebih dari satu.
func (a *App) photoFor(day int, slot string) string {
	var best string
	var bestMod time.Time
	for _, p := range a.photoFiles(day, slot) {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if best == "" || fi.ModTime().After(bestMod) {
			best, bestMod = p, fi.ModTime()
		}
	}
	return best
}

type attendanceBody struct {
	data        []byte
	contentType string
}

func attachPhoto(mw *multipart.Writer, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("file foto kosong")
	}
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if ct == "" {
		ct = "application/octet-stream"
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="photo"; filename="%s"`,
		quoteEscaper.Replace(filepath.Base(path))))
	h.Set("Content-Type", ct)
	part, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = part.Write(data)
	return err
}

// buildAttendanceBody menyusun body multipart di memori supaya bisa dikirim
// ulang saat retry dan supaya kegagalan foto ketahuan sebelum request jalan.
//
// Nama field mengikuti yang divalidasi HRIS: status, photo, date, clock_in /
// clock_out, timezone, plus latitude/longitude/radius. Formatnya sama dengan
// yang dikembalikan server di respons login (date "2006-01-02", jam "15:04").
func buildAttendanceBody(now time.Time, kind string, cc ClockConfig, photo string) (attendanceBody, error) {
	timeField := "clock_in"
	if kind == clockOut {
		timeField = "clock_out"
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fields := [][2]string{
		{"latitude", cc.Latitude},
		{"longitude", cc.Longitude},
		{"radius", strconv.FormatFloat(cc.Radius, 'f', -1, 64)},
		{"status", cc.Status},
		{"date", now.Format("2006-01-02")},
		{timeField, now.Format("15:04")},
		{"timezone", appTimezone},
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return attendanceBody{}, err
		}
	}
	// HRIS mewajibkan foto, jadi kegagalan di sini membatalkan request.
	if err := attachPhoto(mw, photo); err != nil {
		return attendanceBody{}, err
	}
	if err := mw.Close(); err != nil {
		return attendanceBody{}, err
	}
	return attendanceBody{data: buf.Bytes(), contentType: mw.FormDataContentType()}, nil
}

func (a *App) sendAttendance(endpoint, token string, body attendanceBody) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body.data))
	if err != nil {
		return 0, "", err
	}
	req.ContentLength = int64(len(body.data))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", body.contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return resp.StatusCode, string(raw), nil
}

// canRunLocked memutuskan apakah slot boleh dijalankan. Pesan kosong berarti
// "lewati tanpa log" (masih menunggu jeda retry).
func (a *App) canRunLocked(key string, now time.Time) (bool, string) {
	if a.inFlight[key] {
		return false, "sedang berjalan, permintaan diabaikan"
	}
	st, ok := a.lastRun[key]
	if !ok {
		return true, ""
	}
	if st.Done {
		return false, ""
	}
	if st.Attempts >= maxAttempts {
		return false, ""
	}
	if last, ok := st.lastAttemptTime(); ok && now.Sub(last) < backoff(st.Attempts) {
		return false, ""
	}
	return true, ""
}

func (a *App) shouldAttempt(key string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	ok, _ := a.canRunLocked(key, now)
	return ok
}

// claim menandai slot sedang berjalan. Ini yang mencegah dua goroutine
// scheduler mengirim absen dobel untuk slot yang sama.
func (a *App) claim(key string, now time.Time) (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ok, reason := a.canRunLocked(key, now)
	if !ok {
		return false, reason
	}
	a.inFlight[key] = true
	return true, ""
}

func (a *App) finish(now time.Time, key, kind string, done, success bool, msg string) {
	a.mu.Lock()
	st := a.lastRun[key]
	st.Attempts++
	st.Time = now.Format("15:04:05")
	st.LastAttempt = now.Format(time.RFC3339)
	st.Done = done
	st.Message = msg
	a.lastRun[key] = st
	delete(a.inFlight, key)
	a.pruneLastRunLocked(now)
	attempts := st.Attempts
	saveErr := a.saveLastRunLocked()
	a.mu.Unlock()

	if !done && attempts >= maxAttempts {
		msg += fmt.Sprintf(" | gagal %dx, berhenti mencoba hari ini", attempts)
	} else if !done {
		msg += fmt.Sprintf(" | percobaan ke-%d, akan dicoba lagi dalam %s", attempts, backoff(attempts))
	}
	a.addLog(kind, msg, success)
	if saveErr != nil {
		a.addLog(kind, "Gagal menyimpan riwayat: "+saveErr.Error(), false)
	}
}

// abort membatalkan klaim tanpa menaikkan jumlah percobaan.
func (a *App) abort(key string) {
	a.mu.Lock()
	delete(a.inFlight, key)
	a.mu.Unlock()
}

func alreadyRecorded(status int, body string) bool {
	if status == http.StatusConflict {
		return true
	}
	if status < 400 || status >= 500 {
		return false
	}
	b := strings.ToLower(body)
	for _, p := range []string{
		"already", "duplicate", "sudah melakukan", "sudah absen",
		"sudah clock", "telah melakukan", "sudah tercatat",
	} {
		if strings.Contains(b, p) {
			return true
		}
	}
	return false
}

// doAttendanceAt memakai waktu yang dikirim pemanggil supaya scheduler dan
// handler memakai tanggal yang sama persis, termasuk di detik pergantian hari.
func (a *App) doAttendanceAt(now time.Time, kind string) {
	cfg := a.snapshot()
	wd := int(now.Weekday())
	key := now.Format("2006-01-02") + ":" + kind

	if wd == 0 || wd == 6 {
		a.addLog(kind, fmt.Sprintf("%s libur, dilewati", dayNames[wd]), false)
		return
	}
	day, ok := cfg.Days[strconv.Itoa(wd)]
	if !ok || !day.Enabled {
		a.addLog(kind, fmt.Sprintf("%s tidak aktif, dilewati", dayNames[wd]), false)
		return
	}
	if cfg.Email == "" || cfg.Password == "" {
		a.addLog(kind, "Email/password belum diisi", false)
		return
	}

	cc, slot, endpoint := day.ClockIn, "in", cfg.BaseURL+"/api/attendance/clock-in"
	if kind == clockOut {
		cc, slot, endpoint = day.ClockOut, "out", cfg.BaseURL+"/api/attendance/clock-out"
	}
	if err := validateClock(cc); err != nil {
		a.addLog(kind, fmt.Sprintf("Konfigurasi %s tidak valid: %s", dayNames[wd], err.Error()), false)
		return
	}

	// HRIS menolak absen tanpa foto, jadi percuma mengirim dan menghabiskan
	// jatah percobaan. Begitu fotonya diunggah, tick berikutnya akan jalan.
	photo := a.photoFor(wd, slot)
	if photo == "" {
		a.addLog(kind, fmt.Sprintf("Foto %s (%s) belum diunggah, absen tidak dikirim (HRIS mewajibkan foto)",
			dayNames[wd], slot), false)
		return
	}

	claimed, reason := a.claim(key, now)
	if !claimed {
		if reason != "" {
			a.addLog(kind, reason, false)
		}
		return
	}

	body, err := buildAttendanceBody(now, kind, cc, photo)
	if err != nil {
		a.abort(key)
		a.addLog(kind, "Gagal menyiapkan foto "+filepath.Base(photo)+": "+err.Error(), false)
		return
	}
	photoNote := "foto " + filepath.Base(photo)

	token := cfg.Token
	if token == "" {
		token, err = a.refreshToken("")
		if err != nil {
			a.finish(now, key, kind, false, false, "Login gagal: "+err.Error())
			return
		}
	}

	status, raw, err := a.sendAttendance(endpoint, token, body)
	if err != nil {
		a.finish(now, key, kind, false, false, "Request gagal: "+err.Error())
		return
	}
	if status == http.StatusUnauthorized {
		a.addLog(kind, "Token ditolak (401), mencoba login ulang...", false)
		newToken, lerr := a.refreshToken(token)
		if lerr != nil {
			a.finish(now, key, kind, false, false, "Gagal login ulang: "+lerr.Error())
			return
		}
		status, raw, err = a.sendAttendance(endpoint, newToken, body)
		if err != nil {
			a.finish(now, key, kind, false, false, "Retry gagal: "+err.Error())
			return
		}
	}

	success := status >= 200 && status < 300
	done := success || alreadyRecorded(status, raw)
	msg := fmt.Sprintf("HTTP %d | %s | %s", status, photoNote, oneLine(raw, 300))
	a.finish(now, key, kind, done, success, msg)
}

// --------------------------------------------------------------- scheduler

func parseHM(s string) (int, int, bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

func scheduledAt(now time.Time, hm string) (time.Time, bool) {
	h, m, ok := parseHM(hm)
	if !ok {
		return time.Time{}, false
	}
	return time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location()), true
}

// isDue benar bila jam terjadwal sudah lewat tapi masih di dalam jendela susulan.
func isDue(now time.Time, hm string, window time.Duration) bool {
	at, ok := scheduledAt(now, hm)
	if !ok {
		return false
	}
	return !now.Before(at) && !now.After(at.Add(window))
}

func (a *App) scheduler() {
	ticker := time.NewTicker(schedulerInterval)
	defer ticker.Stop()
	a.tick(time.Now())
	for t := range ticker.C {
		a.tick(t)
	}
}

// tick menjalankan slot yang jamnya sudah lewat tapi belum berhasil, selama
// masih di dalam jendela susulan. Jadi absen tetap jalan walau app baru
// dinyalakan lewat dari jam yang dijadwalkan.
func (a *App) tick(now time.Time) {
	cfg := a.snapshot()
	wd := int(now.Weekday())
	if wd == 0 || wd == 6 {
		return
	}
	day, ok := cfg.Days[strconv.Itoa(wd)]
	if !ok || !day.Enabled {
		return
	}
	if cfg.Email == "" || cfg.Password == "" {
		return
	}
	window := time.Duration(clampCatchup(cfg.CatchupMinutes)) * time.Minute
	slots := []struct {
		kind string
		cc   ClockConfig
	}{
		{clockIn, day.ClockIn},
		{clockOut, day.ClockOut},
	}
	for _, s := range slots {
		if !isDue(now, s.cc.Time, window) {
			continue
		}
		key := now.Format("2006-01-02") + ":" + s.kind
		if a.shouldAttempt(key, now) {
			go a.doAttendanceAt(now, s.kind)
		}
	}
}

// ------------------------------------------------------------------ helper

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
}

func detectImageExt(head []byte) (string, bool) {
	switch http.DetectContentType(head) {
	case "image/jpeg":
		return ".jpg", true
	case "image/png":
		return ".png", true
	case "image/webp":
		return ".webp", true
	case "image/gif":
		return ".gif", true
	}
	// HEIC/HEIF (foto iPhone) tidak dikenali DetectContentType.
	if len(head) >= 12 && string(head[4:8]) == "ftyp" {
		switch string(head[8:12]) {
		case "heic", "heix", "hevc", "hevx", "mif1", "msf1", "heim", "heis", "hevm", "hevs":
			return ".heic", true
		}
	}
	return "", false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("gagal menulis respons: %v", err)
	}
}

func errResp(msg string) map[string]string { return map[string]string{"error": msg} }

// sameOrigin menolak request yang dipicu dari situs lain (CSRF). Klien non-browser
// (curl, script) tidak mengirim header ini dan tetap diizinkan.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, errResp("permintaan lintas-origin ditolak"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- handlers

type configResponse struct {
	Email          string               `json:"email"`
	PasswordSet    bool                 `json:"password_set"`
	TokenSet       bool                 `json:"token_set"`
	BaseURL        string               `json:"base_url"`
	CatchupMinutes int                  `json:"catchup_minutes"`
	Days           map[string]DayConfig `json:"days"`
}

type configRequest struct {
	Email          *string              `json:"email"`
	Password       *string              `json:"password"`
	BaseURL        *string              `json:"base_url"`
	CatchupMinutes *int                 `json:"catchup_minutes"`
	Days           map[string]DayConfig `json:"days"`
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "halaman tidak tersedia", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

// handleConfigGet tidak pernah mengirim password/token, hanya penandanya.
func (a *App) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := a.snapshot()
	writeJSON(w, http.StatusOK, configResponse{
		Email:          cfg.Email,
		PasswordSet:    cfg.Password != "",
		TokenSet:       cfg.Token != "",
		BaseURL:        cfg.BaseURL,
		CatchupMinutes: cfg.CatchupMinutes,
		Days:           cfg.Days,
	})
}

func (a *App) handleConfigPost(w http.ResponseWriter, r *http.Request) {
	var req configRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("body tidak valid: "+err.Error()))
		return
	}

	days := map[string]DayConfig{}
	for k, d := range req.Days {
		n, err := strconv.Atoi(k)
		if err != nil || n < 1 || n > 5 {
			writeJSON(w, http.StatusBadRequest, errResp("hari tidak valid: "+k))
			return
		}
		if _, _, ok := parseHM(d.ClockIn.Time); !ok {
			writeJSON(w, http.StatusBadRequest, errResp("jam clock-in "+dayNames[n]+" tidak valid"))
			return
		}
		if _, _, ok := parseHM(d.ClockOut.Time); !ok {
			writeJSON(w, http.StatusBadRequest, errResp("jam clock-out "+dayNames[n]+" tidak valid"))
			return
		}
		d.ClockIn = normalizeClock(d.ClockIn, d.ClockIn.Time)
		d.ClockOut = normalizeClock(d.ClockOut, d.ClockOut.Time)
		days[k] = d
	}

	a.mu.Lock()
	if req.Email != nil {
		email := strings.TrimSpace(*req.Email)
		if email != a.config.Email {
			// Token lama milik akun lain, jangan dipakai lagi.
			a.config.Token = ""
			a.lastVerifyOK = false
			a.lastVerify = time.Time{}
			a.lastLoginError = ""
		}
		a.config.Email = email
	}
	if req.Password != nil && *req.Password != "" {
		a.config.Password = *req.Password
	}
	if req.BaseURL != nil {
		if base := strings.TrimRight(strings.TrimSpace(*req.BaseURL), "/"); base != "" {
			a.config.BaseURL = base
		}
	}
	if req.CatchupMinutes != nil {
		a.config.CatchupMinutes = clampCatchup(*req.CatchupMinutes)
	}
	if len(days) > 0 {
		a.config.Days = days
	}
	err := a.saveConfigLocked()
	a.mu.Unlock()

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errResp("gagal menyimpan config: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) handleTestLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("body tidak valid: "+err.Error()))
		return
	}
	body.Email = strings.TrimSpace(body.Email)
	if body.Email == "" {
		writeJSON(w, http.StatusBadRequest, errResp("email wajib diisi"))
		return
	}
	// Password kosong berarti "pakai yang tersimpan".
	if body.Password == "" {
		cfg := a.snapshot()
		if cfg.Email == body.Email && cfg.Password != "" {
			body.Password = cfg.Password
		} else {
			writeJSON(w, http.StatusBadRequest, errResp("password wajib diisi"))
			return
		}
	}

	token, err := a.login(body.Email, body.Password)
	if err != nil {
		a.mu.Lock()
		a.lastLoginError = err.Error()
		a.lastVerifyOK = false
		a.lastVerify = time.Now()
		a.mu.Unlock()
		a.addLog("test-login", "Login gagal: "+err.Error(), false)
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}

	a.mu.Lock()
	a.config.Email = body.Email
	a.config.Password = body.Password
	a.config.Token = token
	a.lastLoginError = ""
	a.lastVerifyOK = true
	a.lastVerify = time.Now()
	saveErr := a.saveConfigLocked()
	a.mu.Unlock()

	if saveErr != nil {
		a.addLog("test-login", "Login berhasil tapi config gagal disimpan: "+saveErr.Error(), false)
		writeJSON(w, http.StatusInternalServerError, errResp("gagal menyimpan config: "+saveErr.Error()))
		return
	}
	a.addLog("test-login", "Login berhasil, token disimpan", true)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (a *App) handleLogs(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	logs := append([]LogEntry{}, a.logs...)
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

func (a *App) handleImages(w http.ResponseWriter, r *http.Request) {
	entries := []map[string]any{}
	for day := 0; day <= 6; day++ {
		entry := map[string]any{"day": day, "name": dayNames[day]}
		for _, slot := range []string{"in", "out"} {
			path := a.photoFor(day, slot)
			item := map[string]any{"exists": path != ""}
			if path != "" {
				item["filename"] = filepath.Base(path)
				item["url"] = "/images/" + filepath.Base(path)
			}
			entry[slot] = item
		}
		entries = append(entries, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": entries})
}

func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("upload gagal: "+err.Error()))
		return
	}
	defer r.MultipartForm.RemoveAll()

	day, err := strconv.Atoi(r.FormValue("day"))
	if err != nil || day < 0 || day > 6 {
		writeJSON(w, http.StatusBadRequest, errResp("day harus 0-6"))
		return
	}
	slot := r.FormValue("slot")
	if slot != "in" && slot != "out" {
		writeJSON(w, http.StatusBadRequest, errResp("slot harus in/out"))
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("file tidak ditemukan: "+err.Error()))
		return
	}
	defer file.Close()

	head := make([]byte, 512)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		writeJSON(w, http.StatusBadRequest, errResp("gagal membaca file: "+err.Error()))
		return
	}
	ext, ok := detectImageExt(head[:n])
	if !ok {
		writeJSON(w, http.StatusBadRequest, errResp("file bukan gambar yang didukung (jpg/png/webp/gif/heic)"))
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeJSON(w, http.StatusInternalServerError, errResp("gagal membaca ulang file: "+err.Error()))
		return
	}

	tmp, err := os.CreateTemp(a.imagesDir, ".upload-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errResp("gagal membuat file: "+err.Error()))
		return
	}
	tmpName := tmp.Name()
	written, copyErr := io.Copy(tmp, io.LimitReader(file, maxUploadBytes+1))
	closeErr := tmp.Close()
	switch {
	case copyErr != nil:
		os.Remove(tmpName)
		writeJSON(w, http.StatusInternalServerError, errResp("gagal menyimpan file: "+copyErr.Error()))
		return
	case closeErr != nil:
		os.Remove(tmpName)
		writeJSON(w, http.StatusInternalServerError, errResp("gagal menutup file: "+closeErr.Error()))
		return
	case written > maxUploadBytes:
		os.Remove(tmpName)
		writeJSON(w, http.StatusRequestEntityTooLarge, errResp("foto terlalu besar (maks 10 MB)"))
		return
	case written == 0:
		os.Remove(tmpName)
		writeJSON(w, http.StatusBadRequest, errResp("file kosong"))
		return
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		writeJSON(w, http.StatusInternalServerError, errResp("gagal set permission: "+err.Error()))
		return
	}

	// Foto lama dengan ekstensi berbeda harus hilang, kalau tidak foto itu
	// yang justru terkirim ke HRIS.
	dst := filepath.Join(a.imagesDir, fmt.Sprintf("day-%d-%s%s", day, slot, ext))
	for _, old := range a.photoFiles(day, slot) {
		if old != dst {
			os.Remove(old)
		}
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		writeJSON(w, http.StatusInternalServerError, errResp("gagal memasang file: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "filename": filepath.Base(dst)})
}

func (a *App) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	day, err := strconv.Atoi(r.PathValue("day"))
	slot := r.PathValue("slot")
	if err != nil || day < 0 || day > 6 || (slot != "in" && slot != "out") {
		writeJSON(w, http.StatusBadRequest, errResp("parameter tidak valid"))
		return
	}
	for _, p := range a.photoFiles(day, slot) {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusInternalServerError, errResp("gagal menghapus: "+err.Error()))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) handleServeImage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !imageNameRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(a.imagesDir, name)
	if !fileExists(path) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

type slotStatus struct {
	Time     string `json:"time"`
	Done     bool   `json:"done"`
	At       string `json:"at,omitempty"`
	Attempts int    `json:"attempts"`
	Message  string `json:"message,omitempty"`
	Pending  bool   `json:"pending"`
}

func (a *App) slotStatusFor(now time.Time, kind string, cc ClockConfig, enabled bool, window time.Duration) slotStatus {
	key := now.Format("2006-01-02") + ":" + kind
	a.mu.Lock()
	st := a.lastRun[key]
	a.mu.Unlock()

	out := slotStatus{
		Time:     cc.Time,
		Done:     st.Done,
		At:       st.Time,
		Attempts: st.Attempts,
		Message:  st.Message,
	}
	if at, ok := scheduledAt(now, cc.Time); ok && enabled && !st.Done {
		out.Pending = !now.Before(at) && !now.After(at.Add(window))
	}
	return out
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := a.snapshot()
	a.mu.Lock()
	loginErr := a.lastLoginError
	a.mu.Unlock()

	now := time.Now()
	wd := int(now.Weekday())
	day := cfg.Days[strconv.Itoa(wd)]
	enabled := day.Enabled && wd != 0 && wd != 6
	window := time.Duration(clampCatchup(cfg.CatchupMinutes)) * time.Minute
	tokenValid, checked := a.tokenStatus()

	checkedAt := ""
	if !checked.IsZero() {
		checkedAt = checked.Format("15:04:05")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"server_time":      now.Format("2006-01-02 15:04:05"),
		"weekday":          wd,
		"weekday_name":     dayNames[wd],
		"day_enabled":      enabled,
		"catchup_minutes":  clampCatchup(cfg.CatchupMinutes),
		"clock_in":         a.slotStatusFor(now, clockIn, day.ClockIn, enabled, window),
		"clock_out":        a.slotStatusFor(now, clockOut, day.ClockOut, enabled, window),
		"token_set":        cfg.Token != "",
		"token_valid":      tokenValid,
		"token_checked_at": checkedAt,
		"login_error":      loginErr,
		"email":            cfg.Email,
	})
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("GET /api/config", a.handleConfigGet)
	mux.HandleFunc("POST /api/config", a.handleConfigPost)
	mux.HandleFunc("POST /api/test-login", a.handleTestLogin)
	mux.HandleFunc("GET /api/logs", a.handleLogs)
	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("GET /api/images", a.handleImages)
	mux.HandleFunc("POST /api/upload", a.handleUpload)
	mux.HandleFunc("DELETE /api/upload/{day}/{slot}", a.handleDeleteImage)
	mux.HandleFunc("GET /images/{name}", a.handleServeImage)
	return csrfGuard(mux)
}

// ------------------------------------------------------------------- main

func main() {
	if dir := strings.TrimSpace(os.Getenv("DATA_DIR")); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("gagal menyiapkan DATA_DIR %s: %v", dir, err)
		}
		app.configPath = filepath.Join(dir, "config.json")
		app.lastRunPath = filepath.Join(dir, "lastrun.json")
		app.imagesDir = filepath.Join(dir, "images")
	}
	if err := os.MkdirAll(app.imagesDir, 0o755); err != nil {
		log.Fatalf("gagal membuat folder %s: %v", app.imagesDir, err)
	}
	if err := app.loadConfig(); err != nil {
		log.Fatalf("konfigurasi: %v", err)
	}
	if err := app.loadLastRun(); err != nil {
		log.Printf("peringatan: %v (riwayat hari ini dianggap kosong)", err)
	}

	go app.scheduler()
	app.tokenStatus() // panaskan cache status token

	host := os.Getenv("HOST")
	if host == "" {
		host = defaultHost
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}
	addr := net.JoinHostPort(host, port)

	srv := &http.Server{
		Addr:              addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	stopped := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Println("menutup server...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
		close(stopped)
	}()

	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		log.Printf("PERINGATAN: server terbuka di %s. Siapa pun di jaringan ini bisa memicu absen Anda.", host)
	}
	log.Printf("HRIS Auto Clock berjalan di http://%s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
	<-stopped
	log.Println("server berhenti")
}

func init() {
	loc, err := time.LoadLocation(appTimezone)
	if err != nil {
		log.Printf("Gagal memuat timezone %s: %v, pakai UTC+7", appTimezone, err)
		loc = time.FixedZone(appTimezone, 7*3600)
	}
	time.Local = loc

	// Supaya foto iPhone punya Content-Type yang benar saat dikirim ke HRIS.
	mime.AddExtensionType(".heic", "image/heic")
	mime.AddExtensionType(".heif", "image/heif")
}
