package main

import (
	"embed"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/getlantern/systray"
)

//go:embed templates/index.html
var templateFS embed.FS

//go:embed app.ico
var appIconICO []byte

const (
	httpPort   = 8080
	webdavPort = 8081
)

var (
	_galleryURL string
	_conn       *Connector
)

func main() {
	baseDir := baseDirectory()
	_conn = NewConnector(baseDir)
	conn := _conn

	go func() {
		conn.Connect() //nolint
	}()

	// Claim the HTTP port before doing anything else.
	// If it's busy, another instance is already running — bring that one up.
	httpLn, err := net.Listen("tcp", fmt.Sprintf(":%d", httpPort))
	if err != nil {
		openBrowser(fmt.Sprintf("http://localhost:%d", httpPort))
		os.Exit(0)
	}

	srv := newServer(conn, baseDir)
	go http.Serve(httpLn, srv) //nolint

	// WebDAV: best-effort, ignore if port busy.
	if wdLn, err := net.Listen("tcp", fmt.Sprintf(":%d", webdavPort)); err == nil {
		go serveWebDAV(conn, wdLn)
	}

	_galleryURL = fmt.Sprintf("http://localhost:%d", httpPort)
	go func() {
		time.Sleep(1200 * time.Millisecond)
		openBrowser(_galleryURL)
	}()

	systray.Run(onTrayReady, onTrayExit)
}

func baseDirectory() string {
	exe, err := os.Executable()
	if err == nil {
		return filepath.Dir(exe)
	}
	dir, _ := os.Getwd()
	return dir
}

func openBrowser(url string) {
	exec.Command("cmd", "/c", "start", url).Start() //nolint
}

func onTrayReady() {
	systray.SetIcon(appIconICO)
	systray.SetTitle("iPhosyn")
	systray.SetTooltip("iPhosyn")

	mOpen := systray.AddMenuItem("Open Gallery", "Open gallery in browser")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit iPhosyn")

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				openBrowser(_galleryURL)
			case <-mQuit.ClickedCh:
				systray.Quit()
			}
		}
	}()
}

func onTrayExit() {
	_conn.Stop()
	os.Exit(0)
}

