package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/net/webdav"
)

func serveWebDAV(conn *Connector, ln net.Listener) {
	fs := &iphoneWebDAVFS{conn: conn}
	handler := &webdav.Handler{
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(),
	}
	http.Serve(ln, handler) //nolint
}

// iphoneWebDAVFS exposes iPhone DCIM over WebDAV.
// MTP backend: stubs return empty — same behaviour as Python version.
type iphoneWebDAVFS struct {
	conn *Connector
}

func (f *iphoneWebDAVFS) Mkdir(_ context.Context, _ string, _ os.FileMode) error {
	return os.ErrPermission
}
func (f *iphoneWebDAVFS) RemoveAll(_ context.Context, _ string) error {
	return os.ErrPermission
}
func (f *iphoneWebDAVFS) Rename(_ context.Context, _, _ string) error {
	return os.ErrPermission
}

func (f *iphoneWebDAVFS) Stat(_ context.Context, name string) (os.FileInfo, error) {
	if name == "/" || name == "" {
		return &emptyDirInfo{name: "/"}, nil
	}
	return nil, os.ErrNotExist
}

func (f *iphoneWebDAVFS) OpenFile(_ context.Context, name string, _ int, _ os.FileMode) (webdav.File, error) {
	if name == "/" || name == "" {
		return &emptyDir{}, nil
	}
	return nil, os.ErrNotExist
}

// ── emptyDir ─────────────────────────────────────────────────────────────────

type emptyDir struct{}

func (d *emptyDir) Close() error                                  { return nil }
func (d *emptyDir) Read(_ []byte) (int, error)                    { return 0, os.ErrInvalid }
func (d *emptyDir) Seek(_ int64, _ int) (int64, error)            { return 0, os.ErrInvalid }
func (d *emptyDir) Readdir(_ int) ([]os.FileInfo, error)          { return nil, nil }
func (d *emptyDir) Stat() (os.FileInfo, error)                    { return &emptyDirInfo{name: "/"}, nil }
func (d *emptyDir) Write(_ []byte) (int, error)                   { return 0, os.ErrPermission }

// ── emptyDirInfo ─────────────────────────────────────────────────────────────

type emptyDirInfo struct{ name string }

func (i *emptyDirInfo) Name() string      { return i.name }
func (i *emptyDirInfo) Size() int64       { return 0 }
func (i *emptyDirInfo) Mode() os.FileMode { return os.ModeDir | 0o555 }
func (i *emptyDirInfo) ModTime() time.Time { return time.Time{} }
func (i *emptyDirInfo) IsDir() bool       { return true }
func (i *emptyDirInfo) Sys() any          { return nil }
