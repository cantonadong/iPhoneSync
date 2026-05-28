package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/disintegration/imaging"
)

// ── One-shot PowerShell scripts (connect + list) ────────────────────────────
// These run as separate processes; only used once or infrequently.

const psConnect = `
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
$shell = New-Object -ComObject Shell.Application
foreach ($item in $shell.NameSpace(17).Items()) {
    if ($item.Name -match 'iPhone|Apple') { Write-Output $item.Name; exit 0 }
}
exit 1
`

const psList = `
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
$shell = New-Object -ComObject Shell.Application
$iphone = $null
foreach ($item in $shell.NameSpace(17).Items()) {
    if ($item.Name -match 'iPhone|Apple') { $iphone = $item; break }
}
if (-not $iphone) { Write-Output '[]'; exit 0 }

function Find-DCIM($folder) {
    foreach ($item in $folder.Items()) {
        if ($item.Name -eq 'DCIM') { return $item.GetFolder() }
        $sf = $item.GetFolder()
        if ($sf) { foreach ($sub in $sf.Items()) {
            if ($sub.Name -eq 'DCIM') { return $sub.GetFolder() }
        }}
    }
}

$dcim = Find-DCIM($iphone.GetFolder())
if (-not $dcim) { Write-Output '[]'; exit 0 }

$list = [System.Collections.Generic.List[object]]::new()
$pe = @('.jpg','.jpeg','.heic','.heif','.png')
$ve = @('.mp4','.mov','.m4v')
foreach ($album in $dcim.Items()) {
    $af = $album.GetFolder()
    if (-not $af) { continue }
    foreach ($f in $af.Items()) {
        $ext = [IO.Path]::GetExtension($f.Name).ToLower()
        if ($ext -notin ($pe + $ve)) { continue }
        $list.Add([ordered]@{
            path  = "$($album.Name)/$($f.Name)"
            name  = $f.Name
            album = $album.Name
            size  = [long]$f.Size
            mtime = 0
            kind  = if ($ext -in $ve) { 'video' } else { 'photo' }
        })
    }
}
$list | ConvertTo-Json -Compress
`

// ── PS daemon script ────────────────────────────────────────────────────────
// A long-running PowerShell process that keeps Shell.Application and the
// iPhone/DCIM shell-item tree alive between copy calls.
// Protocol: newline-delimited JSON on stdin/stdout.
//   Request:  {"album":"...","file":"...","dest":"..."}
//   Response: {"status":"ok","path":"..."} | {"status":"error","msg":"..."}

const psDaemonScript = `
$ErrorActionPreference = 'SilentlyContinue'
[Console]::InputEncoding  = [System.Text.UTF8Encoding]::new($false)
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)

$shell = New-Object -ComObject Shell.Application
$iphone = $null
foreach ($item in $shell.NameSpace(17).Items()) {
    if ($item.Name -match 'iPhone|Apple') { $iphone = $item; break }
}
if (-not $iphone) {
    [Console]::WriteLine((@{status='error';msg='iPhone not found'} | ConvertTo-Json -Compress))
    exit 1
}

function Find-DCIM($folder) {
    foreach ($item in $folder.Items()) {
        if ($item.Name -eq 'DCIM') { return $item.GetFolder() }
        $sf = $item.GetFolder()
        if ($sf) { foreach ($sub in $sf.Items()) {
            if ($sub.Name -eq 'DCIM') { return $sub.GetFolder() }
        }}
    }
}

$dcim = Find-DCIM($iphone.GetFolder())
[Console]::WriteLine((@{status='ready';name=$iphone.Name} | ConvertTo-Json -Compress))

while ($true) {
    $line = [Console]::ReadLine()
    if ($null -eq $line -or $line -eq 'EXIT') { break }

    try { $req = ConvertFrom-Json $line } catch {
        [Console]::WriteLine((@{status='error';msg='bad json'} | ConvertTo-Json -Compress))
        continue
    }

    # Re-discover DCIM if shell items have expired
    if (-not $dcim) { $dcim = Find-DCIM($iphone.GetFolder()) }
    if (-not $dcim) {
        [Console]::WriteLine((@{status='error';msg='DCIM not accessible'} | ConvertTo-Json -Compress))
        continue
    }

    $albumFolder = $null
    foreach ($a in $dcim.Items()) {
        if ($a.Name -eq $req.album) { $albumFolder = $a.GetFolder(); break }
    }
    if (-not $albumFolder) {
        [Console]::WriteLine((@{status='error';msg="album not found: $($req.album)"} | ConvertTo-Json -Compress))
        continue
    }

    $fileItem = $null
    foreach ($f in $albumFolder.Items()) {
        if ($f.Name -eq $req.file) { $fileItem = $f; break }
    }
    if (-not $fileItem) {
        [Console]::WriteLine((@{status='error';msg="file not found: $($req.file)"} | ConvertTo-Json -Compress))
        continue
    }

    $null = New-Item -ItemType Directory -Path $req.dest -Force
    $destNS = $shell.NameSpace($req.dest)
    $destNS.CopyHere($fileItem, 1044)

    $out = Join-Path $req.dest $req.file
    $ok = $false
    for ($i = 0; $i -lt 90; $i++) {
        if (Test-Path $out) {
            $s1 = (Get-Item $out).Length
            Start-Sleep -Milliseconds 400
            $s2 = (Get-Item $out).Length
            if ($s1 -gt 0 -and $s1 -eq $s2) { $ok = $true; break }
        } else { Start-Sleep -Milliseconds 300 }
    }

    if ($ok) {
        [Console]::WriteLine((@{status='ok';path=$out} | ConvertTo-Json -Compress))
    } else {
        [Console]::WriteLine((@{status='error';msg='copy timed out'} | ConvertTo-Json -Compress))
    }
}
`

// ── psDaemon ────────────────────────────────────────────────────────────────

type psDaemon struct {
	mu     sync.Mutex // serialises every stdin write + stdout read
	cmd    *exec.Cmd
	stdin  *bufio.Writer
	stdout *bufio.Scanner
	tmp    string
	dead   atomic.Bool
}

func startPSDaemon() (*psDaemon, error) {
	f, err := os.CreateTemp("", "iphosyn-daemon-*.ps1")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	if _, err := f.WriteString(psDaemonScript); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, err
	}
	f.Close()

	cmd := exec.Command("powershell",
		"-NoProfile", "-NonInteractive", "-STA",
		"-ExecutionPolicy", "Bypass",
		"-File", tmp)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		os.Remove(tmp)
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		os.Remove(tmp)
		return nil, err
	}
	// discard stderr so it doesn't block the process
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		os.Remove(tmp)
		return nil, err
	}

	d := &psDaemon{
		cmd:    cmd,
		stdin:  bufio.NewWriter(stdinPipe),
		stdout: bufio.NewScanner(stdoutPipe),
		tmp:    tmp,
	}

	// wait for {"status":"ready",...} or {"status":"error",...}
	if !d.stdout.Scan() {
		cmd.Process.Kill() //nolint
		cmd.Wait()         //nolint
		os.Remove(tmp)
		return nil, fmt.Errorf("daemon did not respond on startup")
	}
	var resp map[string]any
	if err := json.Unmarshal(d.stdout.Bytes(), &resp); err != nil {
		cmd.Process.Kill() //nolint
		cmd.Wait()         //nolint
		os.Remove(tmp)
		return nil, fmt.Errorf("daemon startup parse: %w", err)
	}
	if s, _ := resp["status"].(string); s != "ready" {
		cmd.Process.Kill() //nolint
		cmd.Wait()         //nolint
		os.Remove(tmp)
		return nil, fmt.Errorf("daemon: %v", resp["msg"])
	}
	return d, nil
}

// copy sends one copy request to the daemon and waits for its response.
// The daemon's internal mutex guarantees serial MTP access.
func (d *psDaemon) copy(album, file, dest string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	req, _ := json.Marshal(map[string]string{"album": album, "file": file, "dest": dest})
	if _, err := fmt.Fprintf(d.stdin, "%s\n", req); err != nil {
		d.dead.Store(true)
		return "", fmt.Errorf("daemon write: %w", err)
	}
	if err := d.stdin.Flush(); err != nil {
		d.dead.Store(true)
		return "", fmt.Errorf("daemon flush: %w", err)
	}
	if !d.stdout.Scan() {
		d.dead.Store(true)
		return "", fmt.Errorf("daemon closed unexpectedly")
	}
	var resp map[string]any
	if err := json.Unmarshal(d.stdout.Bytes(), &resp); err != nil {
		d.dead.Store(true)
		return "", fmt.Errorf("daemon response parse: %w", err)
	}
	if s, _ := resp["status"].(string); s != "ok" {
		return "", fmt.Errorf("%v", resp["msg"])
	}
	path, _ := resp["path"].(string)
	return path, nil
}

func (d *psDaemon) stop() {
	d.mu.Lock()
	fmt.Fprintln(d.stdin, "EXIT") //nolint
	d.stdin.Flush()               //nolint
	d.mu.Unlock()
	d.cmd.Process.Kill() //nolint
	d.cmd.Wait()         //nolint
	os.Remove(d.tmp)     //nolint
}

// ── Photo struct ────────────────────────────────────────────────────────────

type Photo struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Album string `json:"album"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	Kind  string `json:"kind"`
}

// ── Connector ───────────────────────────────────────────────────────────────

type Connector struct {
	stateMu   sync.Mutex
	connected bool
	name      string

	photosMu sync.Mutex
	photos   []Photo

	daemonMu sync.Mutex
	daemon   *psDaemon

	thumbDir string
	fileDir  string
}

func NewConnector(baseDir string) *Connector {
	c := &Connector{
		thumbDir: filepath.Join(baseDir, ".cache", "thumbs"),
		fileDir:  filepath.Join(baseDir, ".cache", "files"),
	}
	os.MkdirAll(c.thumbDir, 0o755) //nolint
	os.MkdirAll(c.fileDir, 0o755)  //nolint
	return c
}

func (c *Connector) Connect() error {
	out, err := runPS(psConnect)
	if err != nil {
		return fmt.Errorf("iPhone not found: %w", err)
	}
	name := strings.TrimSpace(out)
	if name == "" {
		name = "iPhone"
	}

	// Start the daemon that will handle all copy operations.
	d, err := startPSDaemon()
	if err != nil {
		// daemon error → still mark connected so UI shows device (list works)
		c.stateMu.Lock()
		c.name = name
		c.connected = true
		c.stateMu.Unlock()
		return fmt.Errorf("daemon start: %w", err)
	}

	c.stateMu.Lock()
	c.name = name
	c.connected = true
	c.stateMu.Unlock()

	c.daemonMu.Lock()
	if c.daemon != nil {
		c.daemon.stop()
	}
	c.daemon = d
	c.daemonMu.Unlock()
	return nil
}

func (c *Connector) Stop() {
	c.daemonMu.Lock()
	d := c.daemon
	c.daemon = nil
	c.daemonMu.Unlock()
	if d != nil {
		d.stop()
	}
}

func (c *Connector) Connected() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.connected
}

func (c *Connector) Name() string {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.name
}

func (c *Connector) ListPhotos() ([]Photo, error) {
	c.photosMu.Lock()
	if c.photos != nil {
		p := c.photos
		c.photosMu.Unlock()
		return p, nil
	}
	c.photosMu.Unlock()

	out, err := runPS(psList)
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(out)
	if raw == "" || raw == "[]" {
		c.photosMu.Lock()
		c.photos = []Photo{}
		c.photosMu.Unlock()
		return c.photos, nil
	}

	var data []Photo
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		var single Photo
		if err2 := json.Unmarshal([]byte(raw), &single); err2 != nil {
			return nil, fmt.Errorf("parse photo list: %w", err)
		}
		data = []Photo{single}
	}
	for i, j := 0, len(data)-1; i < j; i, j = i+1, j-1 {
		data[i], data[j] = data[j], data[i]
	}

	c.photosMu.Lock()
	c.photos = data
	c.photosMu.Unlock()
	return data, nil
}

func (c *Connector) InvalidatePhotos() {
	c.photosMu.Lock()
	c.photos = nil
	c.photosMu.Unlock()
}

// CopyFile copies one file from the iPhone via the persistent daemon.
// All calls are serialised by the daemon's internal mutex → no concurrent MTP.
func (c *Connector) CopyFile(album, fname, destDir string) (string, error) {
	out := filepath.Join(destDir, fname)
	if info, err := os.Stat(out); err == nil && info.Size() > 0 {
		return out, nil
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}

	d, err := c.ensureDaemon()
	if err != nil {
		return "", err
	}

	result, err := d.copy(album, fname, destDir)
	if err != nil {
		// mark daemon dead so next call will restart it
		c.daemonMu.Lock()
		if c.daemon == d {
			c.daemon = nil
		}
		c.daemonMu.Unlock()
		return "", err
	}
	if result == "" {
		result = out
	}
	return result, nil
}

func (c *Connector) ensureDaemon() (*psDaemon, error) {
	c.daemonMu.Lock()
	defer c.daemonMu.Unlock()
	if c.daemon != nil && !c.daemon.dead.Load() {
		return c.daemon, nil
	}
	d, err := startPSDaemon()
	if err != nil {
		return nil, fmt.Errorf("daemon restart: %w", err)
	}
	c.daemon = d
	return d, nil
}

func (c *Connector) ReadFile(path string) ([]byte, error) {
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid path: %s", path)
	}
	album, fname := parts[0], parts[1]
	local, err := c.CopyFile(album, fname, filepath.Join(c.fileDir, album))
	if err != nil {
		return nil, err
	}
	return os.ReadFile(local)
}

func (c *Connector) GetThumbnail(path string, size int) ([]byte, error) {
	thumbPath := filepath.Join(c.thumbDir,
		strings.ReplaceAll(path, "/", string(os.PathSeparator)))
	thumbPath = strings.TrimSuffix(thumbPath, filepath.Ext(thumbPath)) + ".jpg"

	if data, err := os.ReadFile(thumbPath); err == nil {
		return data, nil
	}

	ext := strings.ToLower(filepath.Ext(path))

	if ext == ".mp4" || ext == ".mov" || ext == ".m4v" {
		data := videoThumb(size)
		os.MkdirAll(filepath.Dir(thumbPath), 0o755) //nolint
		os.WriteFile(thumbPath, data, 0o644)         //nolint
		return data, nil
	}

	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid path: %s", path)
	}
	album, fname := parts[0], parts[1]
	tmpDir := filepath.Join(c.fileDir, album, ".tmp")
	local, err := c.CopyFile(album, fname, tmpDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		os.Remove(local)  //nolint
		os.Remove(tmpDir) //nolint
	}()

	var data []byte
	if ext == ".heic" || ext == ".heif" {
		data, err = heicThumbnailPython(local, size)
		if err != nil {
			return nil, fmt.Errorf("heic thumb %s: %w", path, err)
		}
	} else {
		imgData, err := os.ReadFile(local)
		if err != nil {
			return nil, err
		}
		img, _, err := image.Decode(bytes.NewReader(imgData))
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		thumb := imaging.Thumbnail(img, size, size, imaging.Lanczos)
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: 80}); err != nil {
			return nil, err
		}
		data = buf.Bytes()
	}

	os.MkdirAll(filepath.Dir(thumbPath), 0o755) //nolint
	os.WriteFile(thumbPath, data, 0o644)         //nolint
	return data, nil
}

// ── Image helpers ───────────────────────────────────────────────────────────

// heicThumbnailPython uses Python + pillow_heif to decode a HEIC file.
func heicThumbnailPython(heicPath string, size int) ([]byte, error) {
	outPath := heicPath + ".thumb.jpg"
	defer os.Remove(outPath) //nolint

	script := fmt.Sprintf(`
import pillow_heif
pillow_heif.register_heif_opener()
from PIL import Image
img = Image.open(r'%s').convert('RGB')
img.thumbnail((%d, %d))
img.save(r'%s', 'JPEG', quality=80)
`, heicPath, size, size, outPath)

	cmd := exec.Command("python", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return os.ReadFile(outPath)
}

func videoThumb(size int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{30, 30, 30, 255}}, image.Point{}, draw.Src)
	cx, cy, r := size/2, size/2, size/5
	for y := cy - r; y <= cy+r; y++ {
		dy := y - cy
		if dy < -r || dy > r {
			continue
		}
		span := r - abs(dy)
		for x := cx - r; x <= cx-r+span*2; x++ {
			img.SetRGBA(x, y, color.RGBA{180, 180, 180, 255})
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}) //nolint
	return buf.Bytes()
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// ── One-shot PowerShell runner (connect, list, folder-picker) ───────────────

func runPS(script string) (string, error) {
	return runPSArgs(script)
}

func runPSArgs(script string, extraArgs ...string) (string, error) {
	f, err := os.CreateTemp("", "iphosyn-*.ps1")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp) //nolint
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		return "", err
	}
	f.Close()

	args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", tmp}
	args = append(args, extraArgs...)
	cmd := exec.Command("powershell", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
