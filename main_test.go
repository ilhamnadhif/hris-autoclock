package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	monday   = time.Date(2026, 9, 14, 9, 5, 0, 0, time.Local)
	saturday = time.Date(2026, 9, 19, 9, 5, 0, 0, time.Local)
)

var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1, 2, 3, 4}, 40)...)
var jpegBytes = append([]byte("\xff\xd8\xff\xe0"), bytes.Repeat([]byte{9}, 100)...)

func newTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	a := newApp()
	a.configPath = filepath.Join(dir, "config.json")
	a.lastRunPath = filepath.Join(dir, "lastrun.json")
	a.imagesDir = filepath.Join(dir, "images")
	if err := os.MkdirAll(a.imagesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	a.config = defaultConfig()
	a.config.Email = "user@lumoshive.com"
	a.config.Password = "rahasia"
	a.config.Token = "tok1"
	return a
}

// putFoto memasang foto untuk hari/slot tertentu; HRIS mewajibkan foto.
func (a *App) putFoto(t *testing.T, day int, slot string) {
	t.Helper()
	p := filepath.Join(a.imagesDir, fmt.Sprintf("day-%d-%s.png", day, slot))
	if err := os.WriteFile(p, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (a *App) stateOf(t *testing.T, now time.Time, kind string) RunState {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastRun[now.Format("2006-01-02")+":"+kind]
}

func (a *App) lastLog(t *testing.T) LogEntry {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.logs) == 0 {
		t.Fatal("tidak ada log")
	}
	return a.logs[len(a.logs)-1]
}

// ------------------------------------------------------------- unit tests

func TestRunStateUnmarshalLegacyAndNew(t *testing.T) {
	var state map[string]RunState
	if err := json.Unmarshal([]byte(`{"2026-08-11:clock-in":"09:00:13"}`), &state); err != nil {
		t.Fatalf("format lama gagal dibaca: %v", err)
	}
	if got := state["2026-08-11:clock-in"]; !got.Done || got.Time != "09:00:13" {
		t.Fatalf("format lama salah dipetakan: %+v", got)
	}
	if err := json.Unmarshal([]byte(`{"x":{"time":"09:00:01","done":false,"attempts":3}}`), &state); err != nil {
		t.Fatalf("format baru gagal dibaca: %v", err)
	}
	if got := state["x"]; got.Done || got.Attempts != 3 {
		t.Fatalf("format baru salah dipetakan: %+v", got)
	}
	if err := json.Unmarshal([]byte(`{"x":123}`), &state); err == nil {
		t.Fatal("angka seharusnya ditolak")
	}
}

func TestBackoffNaikDanDibatasi(t *testing.T) {
	want := []time.Duration{0, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, w := range want {
		if got := backoff(i); got != w {
			t.Errorf("backoff(%d) = %v, mau %v", i, got, w)
		}
	}
	if got := backoff(maxAttempts + 50); got != 15*time.Minute {
		t.Errorf("backoff besar = %v, harus tetap 15m (tidak overflow)", got)
	}
}

func TestDetectImageExt(t *testing.T) {
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP"), bytes.Repeat([]byte{0}, 20)...)
	heic := append([]byte("\x00\x00\x00\x18ftypheic"), bytes.Repeat([]byte{0}, 20)...)
	cases := []struct {
		name string
		in   []byte
		ext  string
		ok   bool
	}{
		{"png", pngBytes, ".png", true},
		{"jpeg", jpegBytes, ".jpg", true},
		{"gif", []byte("GIF89a0123456789"), ".gif", true},
		{"webp", webp, ".webp", true},
		{"heic", heic, ".heic", true},
		{"teks", []byte("halo ini bukan gambar"), "", false},
		{"kosong", nil, "", false},
		{"elf", []byte("\x7fELF\x02\x01\x01\x00blahblah"), "", false},
	}
	for _, c := range cases {
		ext, ok := detectImageExt(c.in)
		if ok != c.ok || ext != c.ext {
			t.Errorf("%s: dapat (%q,%v) mau (%q,%v)", c.name, ext, ok, c.ext, c.ok)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	mk := func(headers map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://localhost:8787/api/trigger/clock-in", nil)
		r.Host = "localhost:8787"
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}
	cases := []struct {
		name string
		h    map[string]string
		want bool
	}{
		{"tanpa header (curl)", nil, true},
		{"fetch dari halaman sendiri", map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		{"diketik langsung di address bar", map[string]string{"Sec-Fetch-Site": "none"}, true},
		{"situs jahat", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"situs jahat via Origin", map[string]string{"Origin": "https://jahat.example"}, false},
		{"origin sendiri", map[string]string{"Origin": "http://localhost:8787"}, true},
		{"origin rusak", map[string]string{"Origin": "://"}, false},
	}
	for _, c := range cases {
		if got := sameOrigin(mk(c.h)); got != c.want {
			t.Errorf("%s: dapat %v mau %v", c.name, got, c.want)
		}
	}
}

func TestNormalizeConfigMengisiDefault(t *testing.T) {
	cfg := normalizeConfig(Config{
		BaseURL: "  https://hris.example/  ",
		Days: map[string]DayConfig{
			"1": {Enabled: true, ClockIn: ClockConfig{Time: "ngawur", Latitude: " -6.1 "}},
		},
	})
	if cfg.BaseURL != "https://hris.example" {
		t.Errorf("base_url = %q", cfg.BaseURL)
	}
	if cfg.CatchupMinutes != defaultCatchupMinutes {
		t.Errorf("catchup = %d", cfg.CatchupMinutes)
	}
	d := cfg.Days["1"]
	if d.ClockIn.Time != "09:00" || d.ClockOut.Time != "18:00" {
		t.Errorf("jam tidak di-default: %+v", d)
	}
	if d.ClockIn.Latitude != "-6.1" || d.ClockIn.Status != "WFH" || d.ClockIn.Radius != defaultRadius {
		t.Errorf("clock_in tidak dinormalkan: %+v", d.ClockIn)
	}
	if empty := normalizeConfig(Config{}); len(empty.Days) != 5 || empty.BaseURL != defaultBaseURL {
		t.Errorf("config kosong tidak dapat default: %+v", empty)
	}
}

func TestAlreadyRecorded(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{200, "ok", false},
		{409, "", true},
		{422, `{"message":"You have already clocked in today"}`, true},
		{400, `{"message":"Anda sudah melakukan clock in"}`, true},
		{500, "internal server error", false},
		{422, `{"message":"photo is required"}`, false},
	}
	for _, c := range cases {
		if got := alreadyRecorded(c.status, c.body); got != c.want {
			t.Errorf("(%d,%q) = %v mau %v", c.status, c.body, got, c.want)
		}
	}
}

func TestOneLineAmanUntukUTF8(t *testing.T) {
	if got := oneLine("  a\n\nb   c ", 100); got != "a b c" {
		t.Errorf("dapat %q", got)
	}
	got := oneLine(strings.Repeat("é", 50), 10)
	if got != strings.Repeat("é", 10)+"..." {
		t.Errorf("pemotongan merusak karakter: %q", got)
	}
}

func TestIsDueJendelaSusulan(t *testing.T) {
	window := 240 * time.Minute
	at := func(h, m int) time.Time { return time.Date(2026, 9, 14, h, m, 0, 0, time.Local) }
	cases := []struct {
		now  time.Time
		want bool
	}{
		{at(8, 59), false}, // belum waktunya
		{at(9, 0), true},   // tepat waktu
		{at(9, 0).Add(30 * time.Second), true},
		{at(11, 30), true}, // telat 2,5 jam -> masih dikejar
		{at(13, 0), true},  // batas persis 4 jam
		{at(13, 1), false}, // lewat jendela
		{at(23, 0), false},
	}
	for _, c := range cases {
		if got := isDue(c.now, "09:00", window); got != c.want {
			t.Errorf("isDue(%s) = %v mau %v", c.now.Format("15:04:05"), got, c.want)
		}
	}
	if isDue(at(10, 0), "bukan-jam", window) {
		t.Error("jam tidak valid tidak boleh dianggap due")
	}
}

func TestClaimHanyaSatuYangMenang(t *testing.T) {
	a := newTestApp(t)
	key := "2026-09-14:clock-in"
	var won int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := a.claim(key, monday); ok {
				atomic.AddInt32(&won, 1)
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d goroutine berhasil klaim, harus tepat 1", won)
	}
}

func TestPruneRiwayatLama(t *testing.T) {
	a := newTestApp(t)
	now := time.Now()
	a.lastRun = map[string]RunState{
		now.Format("2006-01-02") + ":clock-in":                    {Done: true},
		now.AddDate(0, 0, -3).Format("2006-01-02") + ":clock-in":  {Done: true},
		now.AddDate(0, 0, -40).Format("2006-01-02") + ":clock-in": {Done: true},
		"bukan-key-valid": {Done: true},
	}
	a.pruneLastRunLocked(now)
	if len(a.lastRun) != 2 {
		t.Fatalf("sisa %d entri: %v", len(a.lastRun), a.lastRun)
	}
}

func TestWriteFileAtomicPermission(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rahasia.json")
	if err := writeFileAtomic(p, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("permission = %v, mau 0600", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("file sementara tertinggal: %v", entries)
	}
}

// ------------------------------------------------------- integrasi absen

type fakeHRIS struct {
	srv      *httptest.Server
	attempts int32
	logins   int32

	mu       sync.Mutex
	lastForm map[string]string
	photo    string
	photoCT  string
	auth     string

	status    int32
	body      atomic.Value // string
	failFirst int32
}

func newFakeHRIS(t *testing.T) *fakeHRIS {
	t.Helper()
	f := &fakeHRIS{status: 200}
	f.body.Store(`{"message":"ok"}`)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.logins, 1)
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["password"] != "rahasia" {
			w.WriteHeader(401)
			io.WriteString(w, `{"message":"invalid credentials"}`)
			return
		}
		fmt.Fprintf(w, `{"token":"tok%d"}`, atomic.LoadInt32(&f.logins)+1)
	})
	mux.HandleFunc("/api/auth/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `{"id":1}`)
	})
	handleAttendance := func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&f.attempts, 1)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		f.auth = r.Header.Get("Authorization")
		f.lastForm = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			f.lastForm[k] = v[0]
		}
		f.photo, f.photoCT = "", ""
		if fhs := r.MultipartForm.File["photo"]; len(fhs) > 0 {
			file, _ := fhs[0].Open()
			data, _ := io.ReadAll(file)
			file.Close()
			f.photo = string(data)
			f.photoCT = fhs[0].Header.Get("Content-Type")
		}
		f.mu.Unlock()

		if atomic.LoadInt32(&f.failFirst) > 0 && n == 1 {
			w.WriteHeader(401)
			io.WriteString(w, `{"message":"unauthenticated"}`)
			return
		}
		w.WriteHeader(int(atomic.LoadInt32(&f.status)))
		io.WriteString(w, f.body.Load().(string))
	}
	mux.HandleFunc("/api/attendance/clock-in", handleAttendance)
	mux.HandleFunc("/api/attendance/clock-out", handleAttendance)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestAbsenSuksesMengirimSemuaField(t *testing.T) {
	a := newTestApp(t)
	f := newFakeHRIS(t)
	a.config.BaseURL = f.srv.URL
	d := a.config.Days["1"]
	d.ClockIn = ClockConfig{Time: "09:00", Latitude: "-6.5024163", Longitude: "106.8028025", Radius: 37933, Status: "WFH"}
	a.config.Days["1"] = d
	if err := os.WriteFile(filepath.Join(a.imagesDir, "day-1-in.png"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	a.doAttendanceAt(monday, clockIn)

	if atomic.LoadInt32(&f.attempts) != 1 {
		t.Fatalf("%d request terkirim, mau 1", atomic.LoadInt32(&f.attempts))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.auth != "Bearer tok1" {
		t.Errorf("Authorization = %q", f.auth)
	}
	// Nama & format field ini yang divalidasi HRIS (lihat error 422 aslinya).
	want := map[string]string{
		"latitude":  "-6.5024163",
		"longitude": "106.8028025",
		"radius":    "37933",
		"status":    "WFH",
		"date":      "2026-09-14",
		"clock_in":  "09:05",
		"timezone":  "Asia/Jakarta",
	}
	for k, v := range want {
		if f.lastForm[k] != v {
			t.Errorf("field %s = %q, mau %q", k, f.lastForm[k], v)
		}
	}
	for _, usang := range []string{"status_in", "status_out", "clock_out"} {
		if _, ada := f.lastForm[usang]; ada {
			t.Errorf("field %q tidak boleh ikut dikirim", usang)
		}
	}
	if f.photo != string(pngBytes) {
		t.Errorf("isi foto tidak sama (%d byte terkirim)", len(f.photo))
	}
	if f.photoCT != "image/png" {
		t.Errorf("Content-Type foto = %q, mau image/png", f.photoCT)
	}
	st := a.stateOf(t, monday, clockIn)
	if !st.Done || st.Attempts != 1 {
		t.Errorf("state = %+v", st)
	}
	if !a.lastLog(t).Success {
		t.Errorf("log terakhir harusnya sukses: %+v", a.lastLog(t))
	}
}

func TestAbsenTidakDiulangSetelahSukses(t *testing.T) {
	a := newTestApp(t)
	f := newFakeHRIS(t)
	a.config.BaseURL = f.srv.URL
	a.putFoto(t, 1, "in")
	a.putFoto(t, 3, "in")

	a.doAttendanceAt(monday, clockIn)
	a.mu.Lock()
	logsSetelahSukses := len(a.logs)
	a.mu.Unlock()

	a.doAttendanceAt(monday.Add(time.Minute), clockIn)
	a.doAttendanceAt(monday.Add(time.Hour), clockIn)

	if atomic.LoadInt32(&f.attempts) != 1 {
		t.Fatalf("%d request terkirim, mau 1", atomic.LoadInt32(&f.attempts))
	}
	if !a.stateOf(t, monday, clockIn).Done {
		t.Error("state harus tetap selesai")
	}
	a.mu.Lock()
	n := len(a.logs)
	a.mu.Unlock()
	if n != logsSetelahSukses {
		t.Errorf("percobaan ulang setelah sukses tidak boleh menulis log baru (%d -> %d)", logsSetelahSukses, n)
	}
	// Scheduler pun tidak akan memanggilnya lagi.
	if a.shouldAttempt("2026-09-14:clock-in", monday.Add(time.Hour)) {
		t.Error("slot yang sudah selesai tidak boleh dijadwalkan lagi")
	}
}

func TestAbsenGagalDicobaLagiSetelahBackoff(t *testing.T) {
	a := newTestApp(t)
	f := newFakeHRIS(t)
	a.config.BaseURL = f.srv.URL
	a.putFoto(t, 1, "in")
	a.putFoto(t, 3, "in")
	atomic.StoreInt32(&f.status, 500)
	f.body.Store("server error")

	a.doAttendanceAt(monday, clockIn)
	st := a.stateOf(t, monday, clockIn)
	if st.Done {
		t.Fatal("gagal 500 tidak boleh ditandai selesai")
	}
	if st.Attempts != 1 {
		t.Fatalf("attempts = %d", st.Attempts)
	}
	if a.shouldAttempt("2026-09-14:clock-in", monday.Add(30*time.Second)) {
		t.Error("belum lewat backoff 1 menit, tidak boleh dicoba lagi")
	}
	if !a.shouldAttempt("2026-09-14:clock-in", monday.Add(2*time.Minute)) {
		t.Error("sudah lewat backoff, harus boleh dicoba lagi")
	}

	atomic.StoreInt32(&f.status, 200)
	a.doAttendanceAt(monday.Add(2*time.Minute), clockIn)
	if atomic.LoadInt32(&f.attempts) != 2 {
		t.Fatalf("%d request, mau 2", atomic.LoadInt32(&f.attempts))
	}
	if st = a.stateOf(t, monday, clockIn); !st.Done || st.Attempts != 2 {
		t.Fatalf("state akhir = %+v", st)
	}
}

func TestAbsenBerhentiSetelahBatasPercobaan(t *testing.T) {
	a := newTestApp(t)
	f := newFakeHRIS(t)
	a.config.BaseURL = f.srv.URL
	a.putFoto(t, 1, "in")
	a.putFoto(t, 3, "in")
	atomic.StoreInt32(&f.status, 500)

	now := monday
	for i := 0; i < maxAttempts+3; i++ {
		a.doAttendanceAt(now, clockIn)
		now = now.Add(20 * time.Minute)
	}
	if int(atomic.LoadInt32(&f.attempts)) != maxAttempts {
		t.Fatalf("%d request, mau berhenti di %d", atomic.LoadInt32(&f.attempts), maxAttempts)
	}
	if !strings.Contains(a.lastLog(t).Message, "berhenti mencoba") {
		t.Errorf("log terakhir = %q", a.lastLog(t).Message)
	}
}

func TestAbsenDitolakServerKarenaSudahAbsen(t *testing.T) {
	a := newTestApp(t)
	f := newFakeHRIS(t)
	a.config.BaseURL = f.srv.URL
	a.putFoto(t, 1, "in")
	a.putFoto(t, 3, "in")
	atomic.StoreInt32(&f.status, 422)
	f.body.Store(`{"message":"Anda sudah melakukan clock in hari ini"}`)

	a.doAttendanceAt(monday, clockIn)
	a.doAttendanceAt(monday.Add(30*time.Minute), clockIn)

	if atomic.LoadInt32(&f.attempts) != 1 {
		t.Fatalf("%d request, mau 1 (tidak perlu diulang)", atomic.LoadInt32(&f.attempts))
	}
	if st := a.stateOf(t, monday, clockIn); !st.Done {
		t.Error("harus ditandai selesai supaya tidak dicoba terus")
	}
}

func TestAbsenLoginUlangSaat401(t *testing.T) {
	a := newTestApp(t)
	f := newFakeHRIS(t)
	a.config.BaseURL = f.srv.URL
	a.putFoto(t, 1, "in")
	a.putFoto(t, 3, "in")
	atomic.StoreInt32(&f.failFirst, 1)

	a.doAttendanceAt(monday, clockIn)

	if atomic.LoadInt32(&f.attempts) != 2 {
		t.Fatalf("%d request, mau 2 (asli + retry)", atomic.LoadInt32(&f.attempts))
	}
	if atomic.LoadInt32(&f.logins) != 1 {
		t.Fatalf("%d login, mau 1", atomic.LoadInt32(&f.logins))
	}
	f.mu.Lock()
	auth := f.auth
	f.mu.Unlock()
	if auth != "Bearer tok2" {
		t.Errorf("retry memakai token lama: %q", auth)
	}
	if cfg := a.snapshot(); cfg.Token != "tok2" {
		t.Errorf("token baru tidak disimpan: %q", cfg.Token)
	}
	if st := a.stateOf(t, monday, clockIn); !st.Done {
		t.Error("harusnya selesai setelah retry")
	}
}

func TestAbsenBanyakGoroutineHanyaKirimSekali(t *testing.T) {
	a := newTestApp(t)
	f := newFakeHRIS(t)
	a.config.BaseURL = f.srv.URL
	a.putFoto(t, 1, "in")
	a.putFoto(t, 3, "in")

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.doAttendanceAt(monday, clockIn)
		}()
	}
	wg.Wait()
	if atomic.LoadInt32(&f.attempts) != 1 {
		t.Fatalf("%d request terkirim, mau tepat 1", atomic.LoadInt32(&f.attempts))
	}
}

func TestAbsenDilewatiSaatAkhirPekanDanHariMati(t *testing.T) {
	a := newTestApp(t)
	f := newFakeHRIS(t)
	a.config.BaseURL = f.srv.URL

	a.doAttendanceAt(saturday, clockIn)
	if atomic.LoadInt32(&f.attempts) != 0 {
		t.Fatal("akhir pekan tidak boleh mengirim absen")
	}

	d := a.config.Days["1"]
	d.Enabled = false
	a.config.Days["1"] = d
	a.doAttendanceAt(monday, clockIn)
	if atomic.LoadInt32(&f.attempts) != 0 {
		t.Fatal("hari nonaktif tidak boleh mengirim absen")
	}
}

func TestAbsenBerhentiSaatKoordinatTidakValid(t *testing.T) {
	a := newTestApp(t)
	f := newFakeHRIS(t)
	a.config.BaseURL = f.srv.URL
	d := a.config.Days["1"]
	d.ClockIn.Latitude = "-6.50,"
	a.config.Days["1"] = d

	a.doAttendanceAt(monday, clockIn)
	if atomic.LoadInt32(&f.attempts) != 0 {
		t.Fatal("koordinat rusak tidak boleh dikirim ke HRIS")
	}
	if !strings.Contains(a.lastLog(t).Message, "latitude") {
		t.Errorf("log = %q", a.lastLog(t).Message)
	}
	if st := a.stateOf(t, monday, clockIn); st.Attempts != 0 {
		t.Error("konfigurasi rusak tidak boleh menghabiskan jatah percobaan")
	}
}

func TestAbsenTidakDikirimTanpaFoto(t *testing.T) {
	a := newTestApp(t)
	f := newFakeHRIS(t)
	a.config.BaseURL = f.srv.URL

	a.doAttendanceAt(monday, clockIn)
	if atomic.LoadInt32(&f.attempts) != 0 {
		t.Fatalf("HRIS mewajibkan foto, request tidak boleh dikirim (%d terkirim)", atomic.LoadInt32(&f.attempts))
	}
	if !strings.Contains(a.lastLog(t).Message, "belum diunggah") {
		t.Errorf("log = %q", a.lastLog(t).Message)
	}
	if st := a.stateOf(t, monday, clockIn); st.Attempts != 0 {
		t.Error("foto hilang tidak boleh menghabiskan jatah percobaan")
	}

	// Begitu foto dipasang, percobaan berikutnya jalan.
	a.putFoto(t, 1, "in")
	a.doAttendanceAt(monday.Add(time.Minute), clockIn)
	if atomic.LoadInt32(&f.attempts) != 1 {
		t.Fatalf("setelah foto ada, absen harus terkirim (%d)", atomic.LoadInt32(&f.attempts))
	}
}

func TestAbsenBatalSaatFotoTidakTerbaca(t *testing.T) {
	a := newTestApp(t)
	f := newFakeHRIS(t)
	a.config.BaseURL = f.srv.URL
	p := filepath.Join(a.imagesDir, "day-1-in.png")
	if err := os.WriteFile(p, pngBytes, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p, 0o644)

	a.doAttendanceAt(monday, clockIn)
	msg := a.lastLog(t).Message
	if !strings.Contains(msg, "Gagal menyiapkan foto") {
		t.Errorf("log harus jujur soal foto gagal, dapat: %q", msg)
	}
	if atomic.LoadInt32(&f.attempts) != 0 {
		t.Fatal("absen tanpa foto akan ditolak HRIS, jangan dikirim")
	}
	if st := a.stateOf(t, monday, clockIn); st.Attempts != 0 {
		t.Error("foto rusak tidak boleh menghabiskan jatah percobaan")
	}
}

// -------------------------------------------------------------- handlers

func doReq(t *testing.T, h http.Handler, method, target string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, body)
	r.Host = "localhost:8787"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func jsonHeaders() map[string]string {
	return map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "same-origin"}
}

func TestConfigGetTidakMembocorkanRahasia(t *testing.T) {
	a := newTestApp(t)
	w := doReq(t, a.routes(), http.MethodGet, "/api/config", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, secret := range []string{"rahasia", "tok1"} {
		if strings.Contains(body, secret) {
			t.Fatalf("respons membocorkan %q: %s", secret, body)
		}
	}
	var res configResponse
	json.Unmarshal([]byte(body), &res)
	if !res.PasswordSet || !res.TokenSet || res.Email != "user@lumoshive.com" {
		t.Fatalf("respons = %+v", res)
	}
}

func TestConfigPostMempertahankanPasswordDanBaseURL(t *testing.T) {
	a := newTestApp(t)
	a.config.BaseURL = "https://hris.internal"
	payload := `{"days":{"1":{"enabled":true,"clock_in":{"time":"08:30","latitude":"-6.1","longitude":"106.7","radius":200,"status":"WFH"},"clock_out":{"time":"17:30","latitude":"-6.1","longitude":"106.7","radius":200,"status":"WFH"}}}}`
	w := doReq(t, a.routes(), http.MethodPost, "/api/config", strings.NewReader(payload), jsonHeaders())
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	cfg := a.snapshot()
	if cfg.Password != "rahasia" || cfg.Token != "tok1" {
		t.Errorf("kredensial ikut terhapus: %+v", cfg)
	}
	if cfg.BaseURL != "https://hris.internal" {
		t.Errorf("base_url ter-reset: %q", cfg.BaseURL)
	}
	if cfg.Days["1"].ClockIn.Time != "08:30" {
		t.Errorf("jam tidak tersimpan: %+v", cfg.Days["1"])
	}
	saved, err := os.ReadFile(a.configPath)
	if err != nil || !strings.Contains(string(saved), "08:30") {
		t.Errorf("config tidak ditulis ke disk: %v", err)
	}
}

func TestConfigPostMenolakJamDanHariNgawur(t *testing.T) {
	a := newTestApp(t)
	h := a.routes()
	bad := []string{
		`{"days":{"9":{"enabled":true,"clock_in":{"time":"08:30"},"clock_out":{"time":"17:30"}}}}`,
		`{"days":{"1":{"enabled":true,"clock_in":{"time":""},"clock_out":{"time":"17:30"}}}}`,
		`{"days":{"1":{"enabled":true,"clock_in":{"time":"25:00"},"clock_out":{"time":"17:30"}}}}`,
		`bukan json`,
	}
	for _, p := range bad {
		if w := doReq(t, h, http.MethodPost, "/api/config", strings.NewReader(p), jsonHeaders()); w.Code != 400 {
			t.Errorf("payload %s -> status %d, mau 400", p, w.Code)
		}
	}
	if cfg := a.snapshot(); cfg.Days["1"].ClockIn.Time != "09:00" {
		t.Errorf("config ikut berubah padahal ditolak: %+v", cfg.Days["1"])
	}
}

func TestConfigPostGantiEmailMenghapusToken(t *testing.T) {
	a := newTestApp(t)
	w := doReq(t, a.routes(), http.MethodPost, "/api/config",
		strings.NewReader(`{"email":"orang.lain@lumoshive.com"}`), jsonHeaders())
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if cfg := a.snapshot(); cfg.Token != "" {
		t.Errorf("token akun lama masih dipakai: %q", cfg.Token)
	}
}

func TestCSRFDitolak(t *testing.T) {
	a := newTestApp(t)
	h := a.routes()
	cases := []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Origin": "https://jahat.example"},
	}
	for _, hdr := range cases {
		hdr["Content-Type"] = "application/json"
		w := doReq(t, h, http.MethodPost, "/api/config", strings.NewReader(`{}`), hdr)
		if w.Code != http.StatusForbidden {
			t.Errorf("header %v -> status %d, mau 403", hdr, w.Code)
		}
	}
	w := doReq(t, h, http.MethodGet, "/api/status", nil, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if w.Code != 200 {
		t.Errorf("GET lintas-origin tidak perlu diblokir (tidak mengubah apa pun), status %d", w.Code)
	}
}

func TestUploadMenggantiFotoLamaBerekstensiBeda(t *testing.T) {
	a := newTestApp(t)
	lama := filepath.Join(a.imagesDir, "day-1-in.jpeg")
	if err := os.WriteFile(lama, jpegBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("day", "1")
	mw.WriteField("slot", "in")
	fw, _ := mw.CreateFormFile("file", "foto.png")
	fw.Write(pngBytes)
	mw.Close()

	w := doReq(t, a.routes(), http.MethodPost, "/api/upload", &buf,
		map[string]string{"Content-Type": mw.FormDataContentType(), "Sec-Fetch-Site": "same-origin"})
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(lama); !os.IsNotExist(err) {
		t.Fatal("foto lama .jpeg masih ada, foto itu yang akan terkirim ke HRIS")
	}
	if got := a.photoFor(1, "in"); filepath.Base(got) != "day-1-in.png" {
		t.Fatalf("foto aktif = %q", got)
	}
	files := a.photoFiles(1, "in")
	if len(files) != 1 {
		t.Fatalf("ada %d file tersisa: %v", len(files), files)
	}
}

func TestUploadMenolakFileBukanGambar(t *testing.T) {
	a := newTestApp(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("day", "1")
	mw.WriteField("slot", "in")
	fw, _ := mw.CreateFormFile("file", "virus.jpg")
	fw.Write([]byte("#!/bin/sh\nrm -rf /\n"))
	mw.Close()

	w := doReq(t, a.routes(), http.MethodPost, "/api/upload", &buf,
		map[string]string{"Content-Type": mw.FormDataContentType(), "Sec-Fetch-Site": "same-origin"})
	if w.Code != 400 {
		t.Fatalf("status %d, mau 400", w.Code)
	}
	if files := a.photoFiles(1, "in"); len(files) != 0 {
		t.Fatalf("file sampah tersimpan: %v", files)
	}
	entries, _ := os.ReadDir(a.imagesDir)
	if len(entries) != 0 {
		t.Fatalf("file sementara tertinggal: %v", entries)
	}
}

func TestHapusDanSajikanFoto(t *testing.T) {
	a := newTestApp(t)
	h := a.routes()
	os.WriteFile(filepath.Join(a.imagesDir, "day-2-out.png"), pngBytes, 0o644)

	if w := doReq(t, h, http.MethodGet, "/images/day-2-out.png", nil, nil); w.Code != 200 {
		t.Fatalf("serve foto status %d", w.Code)
	}
	for _, bad := range []string{"/images/..%2fconfig.json", "/images/config.json", "/images/day-2-out", "/images/"} {
		if w := doReq(t, h, http.MethodGet, bad, nil, nil); w.Code == 200 {
			t.Errorf("%s tidak boleh dilayani", bad)
		}
	}
	if w := doReq(t, h, http.MethodDelete, "/api/upload/2/out", nil, map[string]string{"Sec-Fetch-Site": "same-origin"}); w.Code != 200 {
		t.Fatalf("hapus status %d", w.Code)
	}
	if len(a.photoFiles(2, "out")) != 0 {
		t.Fatal("foto tidak terhapus")
	}
	if w := doReq(t, h, http.MethodDelete, "/api/upload/9/out", nil, map[string]string{"Sec-Fetch-Site": "same-origin"}); w.Code != 400 {
		t.Error("day di luar 0-6 harus ditolak")
	}
}

func TestStatusMenampilkanKeadaanHariIni(t *testing.T) {
	a := newTestApp(t)
	a.config.Token = ""
	w := doReq(t, a.routes(), http.MethodGet, "/api/status", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var res map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"server_time", "weekday_name", "day_enabled", "clock_in", "clock_out", "token_set", "catchup_minutes"} {
		if _, ok := res[k]; !ok {
			t.Errorf("field %q hilang dari /api/status", k)
		}
	}
	if res["token_set"].(bool) {
		t.Error("token_set harus false")
	}
	if strings.Contains(w.Body.String(), "rahasia") {
		t.Error("status membocorkan password")
	}
}

func TestMethodTidakDiizinkan(t *testing.T) {
	a := newTestApp(t)
	w := doReq(t, a.routes(), http.MethodPut, "/api/config", nil, map[string]string{"Sec-Fetch-Site": "same-origin"})
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, mau 405", w.Code)
	}
}

func TestLoadConfigDanLastRunDariDisk(t *testing.T) {
	a := newTestApp(t)
	os.WriteFile(a.configPath, []byte(`{"email":"x@y.z","password":"p","token":"t","days":{"1":{"enabled":true,"clock_in":{"time":"07:00"},"clock_out":{"time":"16:00"}}}}`), 0o600)
	os.WriteFile(a.lastRunPath, []byte(`{"`+time.Now().Format("2006-01-02")+`:clock-in":"09:00:13"}`), 0o644)

	if err := a.loadConfig(); err != nil {
		t.Fatal(err)
	}
	if err := a.loadLastRun(); err != nil {
		t.Fatal(err)
	}
	cfg := a.snapshot()
	if cfg.BaseURL != defaultBaseURL || cfg.CatchupMinutes != defaultCatchupMinutes {
		t.Errorf("default tidak terisi: %+v", cfg)
	}
	if cfg.Days["1"].ClockIn.Status != "WFH" || cfg.Days["1"].ClockIn.Radius != defaultRadius {
		t.Errorf("clock_in tidak dinormalkan: %+v", cfg.Days["1"].ClockIn)
	}
	st := a.stateOf(t, time.Now(), clockIn)
	if !st.Done {
		t.Error("riwayat format lama harus dianggap selesai (jangan absen dobel)")
	}
	// Entri basi harus hilang dari disk, bukan cuma dari memori.
	os.WriteFile(a.lastRunPath, []byte(`{"2020-01-01:clock-in":"09:00:00"}`), 0o644)
	if err := a.loadLastRun(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(a.lastRunPath); strings.Contains(string(data), "2020-01-01") {
		t.Error("riwayat lama tidak dibersihkan dari disk")
	}

	// Normalisasi ikut tertulis ke disk (permission + field baru).
	if fi, err := os.Stat(a.configPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config.json tidak dirapatkan jadi 0600: %v", err)
	}
	saved, _ := os.ReadFile(a.configPath)
	if !strings.Contains(string(saved), `"catchup_minutes": 240`) {
		t.Errorf("field baru tidak ditulis ke disk: %s", saved)
	}
	if !strings.Contains(string(saved), `"password": "p"`) {
		t.Error("penulisan ulang config menghilangkan password")
	}
	// File rusak harus dilaporkan, bukan diam-diam menimpa config.
	os.WriteFile(a.configPath, []byte(`{rusak`), 0o600)
	if err := a.loadConfig(); err == nil {
		t.Error("config rusak harus memunculkan error")
	}
}

func TestLoadConfigMembuatDefaultSaatBelumAda(t *testing.T) {
	a := newTestApp(t)
	os.Remove(a.configPath)
	if err := a.loadConfig(); err != nil {
		t.Fatal(err)
	}
	cfg := a.snapshot()
	if len(cfg.Days) != 5 || cfg.BaseURL != defaultBaseURL {
		t.Fatalf("config default tidak lengkap: %+v", cfg)
	}
	fi, err := os.Stat(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config.json permission %v, mau 0600 (berisi password)", fi.Mode().Perm())
	}
}

func TestTickMengejarAbsenYangTerlewat(t *testing.T) {
	a := newTestApp(t)
	f := newFakeHRIS(t)
	a.config.BaseURL = f.srv.URL
	a.putFoto(t, 1, "in")
	a.putFoto(t, 3, "in")

	// App baru nyala jam 11:30, jadwal clock-in 09:00 -> masih dikejar.
	telat := time.Date(2026, 9, 14, 11, 30, 0, 0, time.Local)
	a.tick(telat)
	waitUntil(t, func() bool { return atomic.LoadInt32(&f.attempts) == 1 })

	if st := a.stateOf(t, telat, clockIn); !st.Done {
		t.Fatal("absen susulan gagal")
	}
	// Tick berikutnya tidak boleh mengirim ulang.
	a.tick(telat.Add(20 * time.Second))
	time.Sleep(150 * time.Millisecond)
	if atomic.LoadInt32(&f.attempts) != 1 {
		t.Fatalf("%d request, mau tetap 1", atomic.LoadInt32(&f.attempts))
	}
	// Di luar jendela susulan (default 4 jam) tidak dikirim.
	b := newTestApp(t)
	b.config.BaseURL = f.srv.URL
	b.tick(time.Date(2026, 9, 14, 23, 0, 0, 0, time.Local))
	time.Sleep(150 * time.Millisecond)
	if atomic.LoadInt32(&f.attempts) != 1 {
		t.Fatalf("absen di luar jendela ikut terkirim: %d", atomic.LoadInt32(&f.attempts))
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout menunggu kondisi")
}

func TestLogTidakBanjirSaatPesanBerulang(t *testing.T) {
	a := newTestApp(t)
	f := newFakeHRIS(t)
	a.config.BaseURL = f.srv.URL
	a.putFoto(t, 1, "in")
	d := a.config.Days["1"]
	d.ClockIn.Latitude = "bukan-angka"
	a.config.Days["1"] = d

	a.addLog("penanda", "log lama yang harus tetap terlihat", true)
	for i := 0; i < 500; i++ {
		a.doAttendanceAt(monday.Add(time.Duration(i)*20*time.Second), clockIn)
	}

	a.mu.Lock()
	logs := append([]LogEntry{}, a.logs...)
	a.mu.Unlock()
	if len(logs) != 2 {
		t.Fatalf("ada %d entri log, mau 2 (penanda + 1 pesan digabung)", len(logs))
	}
	if logs[0].Message != "log lama yang harus tetap terlihat" {
		t.Error("log lama terdorong keluar oleh pesan berulang")
	}
	if logs[1].Count != 500 {
		t.Errorf("count = %d, mau 500", logs[1].Count)
	}
	if atomic.LoadInt32(&f.attempts) != 0 {
		t.Fatal("koordinat rusak tidak boleh dikirim")
	}
}

func TestRefreshTokenTidakLoginBerulang(t *testing.T) {
	a := newTestApp(t)
	f := newFakeHRIS(t)
	a.config.BaseURL = f.srv.URL
	a.config.Token = ""

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.refreshToken(""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&f.logins); n != 1 {
		t.Fatalf("%d kali login, mau 1", n)
	}
}
