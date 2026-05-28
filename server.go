package main

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// ── Thumb cache (LRU by insertion, 200 MB cap) ──────────────────────────────

const maxCacheBytes = 200 * 1024 * 1024

type thumbCache struct {
	mu    sync.Mutex
	data  map[string][]byte
	order []string
	total int
}

func newThumbCache() *thumbCache {
	return &thumbCache{data: make(map[string][]byte)}
}

func (c *thumbCache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[key]
	return v, ok
}

func (c *thumbCache) Put(key string, val []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.data[key]; ok {
		c.total -= len(old)
		c.removeOrder(key)
	}
	c.data[key] = val
	c.order = append(c.order, key)
	c.total += len(val)
	for c.total > maxCacheBytes && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if v, ok := c.data[oldest]; ok {
			c.total -= len(v)
			delete(c.data, oldest)
		}
	}
}

func (c *thumbCache) removeOrder(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// ── Server ──────────────────────────────────────────────────────────────────

type Server struct {
	conn    *Connector
	baseDir string
	cfgFile string
	cache   *thumbCache
	mux     *http.ServeMux
}

func newServer(conn *Connector, baseDir string) *Server {
	s := &Server{
		conn:    conn,
		baseDir: baseDir,
		cfgFile: filepath.Join(baseDir, ".cache", "config.json"),
		cache:   newThumbCache(),
		mux:     http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/status", s.handleStatus)
	s.mux.HandleFunc("/api/photos", s.handlePhotos)
	s.mux.HandleFunc("/api/thumbnail", s.handleThumbnail)
	s.mux.HandleFunc("/api/file", s.handleFile)
	s.mux.HandleFunc("/api/download", s.handleDownload)
	s.mux.HandleFunc("/api/config", s.handleConfig)
	s.mux.HandleFunc("/api/pick-folder", s.handlePickFolder)
	s.mux.HandleFunc("/api/refresh", s.handleRefresh)
}

// ── Route handlers ──────────────────────────────────────────────────────────

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := templateFS.ReadFile("templates/index.html")
	if err != nil {
		http.Error(w, "template missing", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data) //nolint
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.conn.Connected() {
		jsonResp(w, map[string]any{"connected": false, "device": "", "count": 0})
		return
	}
	photos, err := s.conn.ListPhotos()
	if err != nil {
		jsonResp(w, map[string]any{"connected": false, "device": "", "count": 0, "error": err.Error()})
		return
	}
	jsonResp(w, map[string]any{"connected": true, "device": s.conn.Name(), "count": len(photos)})
}

func (s *Server) handlePhotos(w http.ResponseWriter, r *http.Request) {
	if !s.conn.Connected() {
		jsonResp(w, []any{})
		return
	}
	photos, err := s.conn.ListPhotos()
	if err != nil {
		w.WriteHeader(500)
		jsonResp(w, map[string]any{"error": err.Error()})
		return
	}
	jsonResp(w, photos)
}

func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.NotFound(w, r)
		return
	}
	if data, ok := s.cache.Get(path); ok {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(data) //nolint
		return
	}
	data, err := s.conn.GetThumbnail(path, 256)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.cache.Put(path, data)
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(data) //nolint
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.NotFound(w, r)
		return
	}
	data, err := s.conn.ReadFile(path)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	ext := strings.ToLower(filepath.Ext(path))
	ct := mime.TypeByExtension(ext)
	if ct == "" {
		ct = "application/octet-stream"
	}
	fname := filepath.Base(path)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, fname))
	w.Write(data) //nolint
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	s.conn.InvalidatePhotos()
	jsonResp(w, map[string]any{"ok": true})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.loadCfg()
		jsonResp(w, map[string]any{"dest": cfg["dest"]})
	case http.MethodPost:
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint
		s.saveCfg(body)
		jsonResp(w, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	cfg := s.loadCfg()
	initial, _ := cfg["dest"].(string)
	if initial == "" {
		initial = `D:\iPhone`
	}
	path := psPickFolder(initial)
	if path != "" {
		s.saveCfg(map[string]any{"dest": path})
	}
	jsonResp(w, map[string]any{"path": path})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Paths []string `json:"paths"`
		Dest  string   `json:"dest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	cfg := s.loadCfg()
	dest := strings.TrimSpace(body.Dest)
	if dest == "" {
		if d, ok := cfg["dest"].(string); ok {
			dest = d
		}
	}
	if dest == "" {
		dest = `D:\iPhone`
	}
	os.MkdirAll(dest, 0o755) //nolint
	s.saveCfg(map[string]any{"dest": dest})

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	total := len(body.Paths)
	saved, failed := 0, 0

	for i, p := range body.Paths {
		fname := filepath.Base(p)
		sendSSE(w, flusher, map[string]any{"i": i, "total": total, "file": fname, "done": false})
		data, err := s.conn.ReadFile(p)
		if err != nil {
			failed++
			continue
		}
		out := uniquePath(filepath.Join(dest, fname))
		if err := os.WriteFile(out, data, 0o644); err != nil {
			failed++
		} else {
			saved++
		}
	}
	sendSSE(w, flusher, map[string]any{
		"i": total, "total": total, "file": "", "done": true,
		"saved": saved, "failed": failed, "dest": dest,
	})
}

// ── Config helpers ──────────────────────────────────────────────────────────

const defaultDest = `D:\iPhone`

func (s *Server) loadCfg() map[string]any {
	data, err := os.ReadFile(s.cfgFile)
	if err != nil {
		return map[string]any{"dest": defaultDest}
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return map[string]any{"dest": defaultDest}
	}
	if d, ok := cfg["dest"].(string); !ok || !validWinPath(d) {
		cfg["dest"] = defaultDest
	}
	return cfg
}

func (s *Server) saveCfg(updates map[string]any) {
	os.MkdirAll(filepath.Dir(s.cfgFile), 0o755) //nolint
	cfg := s.loadCfg()
	for k, v := range updates {
		if k == "dest" {
			if d, ok := v.(string); !ok || !validWinPath(d) {
				continue
			}
		}
		cfg[k] = v
	}
	data, _ := json.Marshal(cfg)
	os.WriteFile(s.cfgFile, data, 0o644) //nolint
}

func validWinPath(p string) bool {
	return len(p) >= 3 && p[1] == ':' && p[2] == '\\'
}

// ── Folder picker ───────────────────────────────────────────────────────────

const psPickFolderScript = `
param([string]$Initial = 'D:\iPhone', [string]$Out = '')
if (-not (Test-Path $Initial -PathType Container)) {
    $Initial = [Environment]::GetFolderPath('MyPictures')
}
Add-Type -AssemblyName System.Windows.Forms
$d = New-Object System.Windows.Forms.OpenFileDialog
$d.Title            = 'Select save folder'
$d.ValidateNames    = $false
$d.CheckFileExists  = $false
$d.CheckPathExists  = $true
$d.FileName         = 'Select_Folder'
$d.InitialDirectory = $Initial
if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) {
    $folder = [IO.Path]::GetDirectoryName($d.FileName)
    if ($folder -and (Test-Path $folder -PathType Container) -and $Out) {
        [IO.File]::WriteAllText($Out, $folder, [Text.Encoding]::UTF8)
    }
}
`

func psPickFolder(initial string) string {
	outFile := filepath.Join(os.TempDir(), fmt.Sprintf("iphosyn_%d.txt", os.Getpid()))
	defer os.Remove(outFile) //nolint

	tmp, err := os.CreateTemp("", "iphosyn-pick-*.ps1")
	if err != nil {
		return ""
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint
	tmp.WriteString(psPickFolderScript) //nolint
	tmp.Close()

	cmd := exec.Command("powershell",
		"-NoProfile", "-STA", "-ExecutionPolicy", "Bypass",
		"-File", tmpPath,
		"-Initial", initial,
		"-Out", outFile,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Run() //nolint

	data, err := os.ReadFile(outFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ── Utilities ───────────────────────────────────────────────────────────────

func jsonResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint
}

func sendSSE(w http.ResponseWriter, f http.Flusher, v any) {
	data, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", data)
	f.Flush()
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s_%d%s", base, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}
