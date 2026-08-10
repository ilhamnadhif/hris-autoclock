package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

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
	Email    string               `json:"email"`
	Password string               `json:"password"`
	Token    string               `json:"token"`
	BaseURL  string               `json:"base_url"`
	Days     map[string]DayConfig `json:"days"`
}

type LogEntry struct {
	Time    string `json:"time"`
	Action  string `json:"action"`
	Message string `json:"message"`
	Success bool   `json:"success"`
}

type App struct {
	mu             sync.Mutex
	config         Config
	logs           []LogEntry
	lastRun        map[string]string
	lastVerify     time.Time
	lastVerifyOK   bool
	lastLoginError string
	configPath     string
	imagesDir      string
}

var app = &App{
	lastRun:    map[string]string{},
	configPath: "config.json",
	imagesDir:  "images",
}

var dayNames = []string{"Minggu", "Senin", "Selasa", "Rabu", "Kamis", "Jumat", "Sabtu"}

func defaultClock() ClockConfig {
	return ClockConfig{
		Time:      "09:00",
		Latitude:  "-6.1648863",
		Longitude: "106.7634657",
		Radius:    150,
		Status:    "WFH",
	}
}

func defaultConfig() Config {
	days := map[string]DayConfig{}
	for i := 1; i <= 5; i++ {
		d := DayConfig{Enabled: true}
		d.ClockIn = defaultClock()
		d.ClockIn.Time = "09:00"
		d.ClockOut = defaultClock()
		d.ClockOut.Time = "18:00"
		days[strconv.Itoa(i)] = d
	}
	return Config{
		BaseURL: "https://hris.lumoshive.com",
		Days:    days,
	}
}

func (a *App) loadConfig() {
	data, err := os.ReadFile(a.configPath)
	if err != nil {
		a.config = Config{}
		a.saveConfig()
		return
	}
	json.Unmarshal(data, &a.config)
	if a.config.BaseURL == "" {
		a.config.BaseURL = "https://hris.lumoshive.com"
	}
	if a.config.Days == nil {
		a.config.Days = defaultConfig().Days
	}
	for k, d := range a.config.Days {
		if d.ClockIn.Status == "" {
			d.ClockIn.Status = "WFH"
		}
		if d.ClockOut.Status == "" {
			d.ClockOut.Status = "WFH"
		}
		a.config.Days[k] = d
	}
}

func (a *App) saveConfig() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.saveConfigLocked()
}

func (a *App) saveConfigLocked() {
	data, _ := json.MarshalIndent(a.config, "", "  ")
	os.WriteFile(a.configPath, data, 0644)
}

func (a *App) addLog(action, msg string, success bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logs = append(a.logs, LogEntry{
		Time:    time.Now().Format("2006-01-02 15:04:05"),
		Action:  action,
		Message: msg,
		Success: success,
	})
	if len(a.logs) > 200 {
		a.logs = a.logs[len(a.logs)-200:]
	}
}

func (a *App) loginErrorMsg(statusCode int, body []byte) string {
	var res struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &res) == nil && res.Message != "" {
		msg := strings.ToLower(res.Message)
		switch {
		case strings.Contains(msg, "credential") || strings.Contains(msg, "invalid"):
			return "invalid credentials"
		case strings.Contains(msg, "unauth"):
			return "unauthorized"
		case strings.Contains(msg, "not found") || strings.Contains(msg, "email"):
			return "email tidak terdaftar"
		case strings.Contains(msg, "blocked") || strings.Contains(msg, "suspend"):
			return "akun diblokir"
		}
		return strings.ToLower(res.Message)
	}
	return fmt.Sprintf("HTTP %d", statusCode)
}

func (a *App) login(email, password string) (string, error) {
	a.mu.Lock()
	baseURL := a.config.BaseURL
	a.mu.Unlock()
	payload, _ := json.Marshal(map[string]string{"email": email, "password": password})
	resp, err := http.Post(strings.TrimRight(baseURL, "/")+"/api/auth/login",
		"application/json", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%s", a.loginErrorMsg(resp.StatusCode, body))
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	if result.Token == "" {
		return "", fmt.Errorf("token kosong")
	}
	return result.Token, nil
}

func (a *App) photoFor(day int, slot string) string {
	matches, _ := filepath.Glob(filepath.Join(a.imagesDir, fmt.Sprintf("day-%d-%s.*", day, slot)))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

func (a *App) sendAttendance(endpoint, token string, cc ClockConfig, statusField string, photo string) (int, string, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer pw.Close()
		mw.WriteField("latitude", cc.Latitude)
		mw.WriteField("longitude", cc.Longitude)
		mw.WriteField("radius", strconv.FormatFloat(cc.Radius, 'f', -1, 64))
		mw.WriteField(statusField, cc.Status)
		if photo != "" {
			fw, err := mw.CreateFormFile("photo", filepath.Base(photo))
			if err == nil {
				f, err := os.Open(photo)
				if err == nil {
					io.Copy(fw, f)
					f.Close()
				}
			}
		}
		mw.Close()
	}()

	req, err := http.NewRequest("POST", endpoint, pr)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(body), nil
}

func (a *App) refreshToken() (string, error) {
	a.mu.Lock()
	email := a.config.Email
	password := a.config.Password
	a.mu.Unlock()
	if email == "" || password == "" {
		return "", fmt.Errorf("email/password kosong")
	}
	token, err := a.login(email, password)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.config.Token = token
	a.saveConfigLocked()
	a.mu.Unlock()
	a.addLog("refresh-token", "Token diperbarui otomatis", true)
	return token, nil
}

func (a *App) dayEnabled(wd int) bool {
	if wd == 0 || wd == 6 {
		return false
	}
	a.mu.Lock()
	day, ok := a.config.Days[strconv.Itoa(wd)]
	a.mu.Unlock()
	return ok && day.Enabled
}

func (a *App) loadLastRun() {
	data, err := os.ReadFile("lastrun.json")
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	json.Unmarshal(data, &a.lastRun)
}

func (a *App) saveLastRun() {
	data, _ := json.Marshal(a.lastRun)
	os.WriteFile("lastrun.json", data, 0644)
}

func (a *App) doAttendance(kind string) {
	a.mu.Lock()
	cfg := a.config
	a.mu.Unlock()

	now := time.Now()
	wd := int(now.Weekday())
	dayKey := strconv.Itoa(wd)
	dateKey := now.Format("2006-01-02")
	key := dateKey + ":" + kind

	if wd == 0 || wd == 6 {
		a.addLog(kind, fmt.Sprintf("%s libur, dilewati", dayNames[wd]), false)
		return
	}

	day, ok := cfg.Days[dayKey]
	if !ok || !day.Enabled {
		a.addLog(kind, fmt.Sprintf("%s tidak aktif, dilewati", dayNames[wd]), false)
		return
	}

	if cfg.Token == "" {
		a.addLog(kind, "Token kosong, lakukan Test Login dulu", false)
		return
	}

	a.mu.Lock()
	last, done := a.lastRun[key]
	a.mu.Unlock()
	if done {
		a.addLog(kind, fmt.Sprintf("Dilewati, sudah dijalankan hari ini pukul %s", last), false)
		return
	}

	var cc ClockConfig
	var endpoint string
	var statusField string
	var slot string
	if kind == "clock-in" {
		cc = day.ClockIn
		slot = "in"
		endpoint = strings.TrimRight(cfg.BaseURL, "/") + "/api/attendance/clock-in"
		statusField = "status_in"
	} else {
		cc = day.ClockOut
		slot = "out"
		endpoint = strings.TrimRight(cfg.BaseURL, "/") + "/api/attendance/clock-out"
		statusField = "status_out"
	}

	photo := a.photoFor(wd, slot)
	photoNote := "tanpa foto"
	if photo != "" {
		photoNote = "foto " + dayNames[wd] + " (" + slot + ")"
	}

	statusCode, body, err := a.sendAttendance(endpoint, cfg.Token, cc, statusField, photo)
	if err != nil {
		a.addLog(kind, "Request gagal: "+err.Error(), false)
		return
	}

	if statusCode == 401 {
		a.addLog(kind, "Token tidak valid (401), mencoba login ulang...", false)
		newToken, err := a.refreshToken()
		if err != nil {
			a.addLog(kind, "Gagal login ulang: "+err.Error(), false)
			return
		}
		statusCode, body, err = a.sendAttendance(endpoint, newToken, cc, statusField, photo)
		if err != nil {
			a.addLog(kind, "Retry gagal: "+err.Error(), false)
			return
		}
	}

	a.mu.Lock()
	a.lastRun[key] = now.Format("15:04:05")
	a.saveLastRun()
	a.mu.Unlock()

	msg := fmt.Sprintf("HTTP %d | %s | %s", statusCode, photoNote, body)
	success := statusCode >= 200 && statusCode < 300
	a.addLog(kind, msg, success)
}

func (a *App) scheduler() {
	for {
		now := time.Now()
		hm := now.Format("15:04")
		wd := strconv.Itoa(int(now.Weekday()))

		a.mu.Lock()
		cfg := a.config
		a.mu.Unlock()

		if cfg.Token != "" {
			if wd != "0" && wd != "6" {
				if day, ok := cfg.Days[wd]; ok && day.Enabled {
					if day.ClockIn.Time == hm {
						go a.doAttendance("clock-in")
					}
					if day.ClockOut.Time == hm {
						go a.doAttendance("clock-out")
					}
				}
			}
		}

		next := time.Now().Add(20 * time.Second)
		time.Sleep(time.Until(next))
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (a *App) verifyToken(token string) bool {
	a.mu.Lock()
	baseURL := a.config.BaseURL
	a.mu.Unlock()
	req, err := http.NewRequest("GET", strings.TrimRight(baseURL, "/")+"/api/auth/user", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == 200
}

func (a *App) cachedTokenValid() bool {
	a.mu.Lock()
	cfg := a.config
	last := a.lastVerify
	lastOK := a.lastVerifyOK
	a.mu.Unlock()
	if cfg.Token == "" {
		return false
	}
	if time.Since(last) < 60*time.Second {
		return lastOK
	}
	valid := a.verifyToken(cfg.Token)
	a.mu.Lock()
	a.lastVerify = time.Now()
	a.lastVerifyOK = valid
	a.mu.Unlock()
	return valid
}

func main() {
	os.MkdirAll(app.imagesDir, 0755)
	app.loadConfig()
	app.loadLastRun()
	go app.scheduler()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	http.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			app.mu.Lock()
			cfg := app.config
			app.mu.Unlock()
			writeJSON(w, 200, cfg)
		case http.MethodPost:
			var cfg Config
			if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			if cfg.BaseURL == "" {
				cfg.BaseURL = "https://hris.lumoshive.com"
			}
			if cfg.Days == nil {
				cfg.Days = map[string]DayConfig{}
			}
			app.mu.Lock()
			app.config.Email = cfg.Email
			app.config.Password = cfg.Password
			app.config.BaseURL = cfg.BaseURL
			app.config.Days = cfg.Days
			app.saveConfigLocked()
			app.mu.Unlock()
			writeJSON(w, 200, map[string]string{"status": "ok"})
		default:
			writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		}
	})

	http.HandleFunc("/api/test-login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
		var body struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Email == "" || body.Password == "" {
			writeJSON(w, 400, map[string]string{"error": "email dan password wajib diisi"})
			return
		}
		token, err := app.login(body.Email, body.Password)
		if err != nil {
			app.mu.Lock()
			app.lastLoginError = err.Error()
			app.mu.Unlock()
			writeJSON(w, 200, map[string]interface{}{"success": false, "error": err.Error()})
			return
		}
		app.mu.Lock()
		app.config.Email = body.Email
		app.config.Password = body.Password
		app.config.Token = token
		app.lastLoginError = ""
		app.saveConfigLocked()
		app.mu.Unlock()
		app.addLog("test-login", "Login berhasil, token disimpan", true)
		writeJSON(w, 200, map[string]interface{}{"success": true, "token": token})
	})

	http.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		logs := append([]LogEntry{}, app.logs...)
		app.mu.Unlock()
		writeJSON(w, 200, map[string]interface{}{"logs": logs})
	})

	http.HandleFunc("/api/images", func(w http.ResponseWriter, r *http.Request) {
		entries := []map[string]interface{}{}
		for day := 0; day <= 6; day++ {
			entry := map[string]interface{}{"day": day, "name": dayNames[day]}
			for _, slot := range []string{"in", "out"} {
				path := app.photoFor(day, slot)
				item := map[string]interface{}{"exists": path != ""}
				if path != "" {
					item["filename"] = filepath.Base(path)
					item["url"] = "/images/" + filepath.Base(path)
				}
				entry[slot] = item
			}
			entries = append(entries, entry)
		}
		writeJSON(w, 200, map[string]interface{}{"images": entries})
	})

	http.HandleFunc("/api/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		day, err := strconv.Atoi(r.FormValue("day"))
		if err != nil || day < 0 || day > 6 {
			writeJSON(w, 400, map[string]string{"error": "day harus 0-6"})
			return
		}
		slot := r.FormValue("slot")
		if slot != "in" && slot != "out" {
			writeJSON(w, 400, map[string]string{"error": "slot harus in/out"})
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		defer file.Close()

		ext := strings.ToLower(filepath.Ext(header.Filename))
		allowed := map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".heic": true}
		if !allowed[ext] {
			ext = ".jpg"
		}
		dst := filepath.Join(app.imagesDir, fmt.Sprintf("day-%d-%s%s", day, slot, ext))
		out, err := os.Create(dst)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		defer out.Close()
		io.Copy(out, file)
		writeJSON(w, 200, map[string]string{"status": "ok", "filename": filepath.Base(dst)})
	})

	http.HandleFunc("/api/upload/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			writeJSON(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/upload/"), "/")
		if len(parts) != 2 {
			writeJSON(w, 400, map[string]string{"error": "format: /api/upload/{day}/{slot}"})
			return
		}
		day, err := strconv.Atoi(parts[0])
		if err != nil || day < 0 || day > 6 || (parts[1] != "in" && parts[1] != "out") {
			writeJSON(w, 400, map[string]string{"error": "parameter tidak valid"})
			return
		}
		matches, _ := filepath.Glob(filepath.Join(app.imagesDir, fmt.Sprintf("day-%d-%s.*", day, parts[1])))
		for _, m := range matches {
			os.Remove(m)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	http.HandleFunc("/images/", func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		path := filepath.Join(app.imagesDir, name)
		if !strings.HasPrefix(name, "day-") || !fileExists(path) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, path)
	})

	http.HandleFunc("/api/trigger/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
		kind := strings.TrimPrefix(r.URL.Path, "/api/trigger/")
		if kind != "clock-in" && kind != "clock-out" {
			writeJSON(w, 400, map[string]string{"error": "invalid trigger"})
			return
		}
		go app.doAttendance(kind)
		writeJSON(w, 200, map[string]string{"status": "running", "action": kind})
	})

	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		cfg := app.config
		lastRun := map[string]string{}
		for k, v := range app.lastRun {
			lastRun[k] = v
		}
		loginErr := app.lastLoginError
		app.mu.Unlock()

		now := time.Now()
		wd := strconv.Itoa(int(now.Weekday()))
		day, _ := cfg.Days[wd]
		writeJSON(w, 200, map[string]interface{}{
			"server_time":    now.Format("2006-01-02 15:04:05"),
			"weekday":        now.Weekday(),
			"weekday_name":   dayNames[int(now.Weekday())],
			"day_enabled":    day.Enabled,
			"clock_in_time":  day.ClockIn.Time,
			"clock_out_time": day.ClockOut.Time,
			"token_set":      cfg.Token != "",
			"token_valid":    app.cachedTokenValid(),
			"login_error":    loginErr,
			"email":          cfg.Email,
			"last_run":       lastRun,
		})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8787"
	}
	log.Printf("HRIS Auto Clock server berjalan di http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
