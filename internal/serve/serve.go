// Package serve is the plumbing both sites share: listening on a Unix socket
// (or TCP outside Docker), server timeouts, security headers, request logs
// and static files. Each site keeps its own pages and its own CSP.
package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"sort"
	"time"
)

// EnvOr returns $name, or def when it's unset or empty.
func EnvOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// Listen opens the Unix socket at sock if it's set (cloudflared on the host
// connects to it), else TCP addr (for running outside Docker).
func Listen(sock, addr string) (net.Listener, string, error) {
	if sock == "" {
		ln, err := net.Listen("tcp", addr)
		return ln, addr, err
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, "", err
	}
	// Who may connect is decided by the directory the socket lives in (on the
	// host): the socket itself is open to anyone who can reach it there.
	if err := os.Chmod(sock, 0o666); err != nil {
		ln.Close()
		return nil, "", err
	}
	return ln, "unix:" + sock, nil
}

// Run serves h on ln until ctx is done, then shuts down gracefully.
func Run(ctx context.Context, ln net.Listener, where string, h http.Handler, log *slog.Logger) error {
	hs := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	log.Info("listening", "on", where)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(shutdown)
}

// Wrap applies the shared middleware: security headers (with the site's
// CSP), request logs and panic recovery.
func Wrap(h http.Handler, csp string, log *slog.Logger) http.Handler {
	return headers(csp, logRequests(log, recoverPanics(log, h)))
}

func headers(csp string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("Cache-Control", "no-store") // pages and errors; static files override it
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		log.Info("request", "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

func recoverPanics(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Error("panic", "path", r.URL.Path, "panic", fmt.Sprint(v))
				http.Error(w, "Something went wrong.", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// RobotsTxt disallows everything: both sites republish other people's public
// data (the rink's schedule, GameSheet's stats), whose own robots.txt files
// keep crawlers out of it.
func RobotsTxt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("User-agent: *\nDisallow: /\n"))
}

// Static serves styles and icons from fsys under /static/. URLs carry
// ?v=<Version>, a content hash, so they can be cached for a day.
type Static struct {
	Version string
	files   http.Handler
}

func NewStatic(fsys fs.FS) (*Static, error) {
	v, err := hashFS(fsys)
	if err != nil {
		return nil, err
	}
	return &Static{Version: v, files: http.FileServerFS(fsys)}, nil
}

// URL returns the cache-busting URL of a static file.
func (s *Static) URL(name string) string { return "/static/" + name + "?v=" + s.Version }

func (s *Static) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch path.Ext(r.URL.Path) {
	case ".css", ".svg", ".png":
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.StripPrefix("/static", s.files).ServeHTTP(w, r)
}

func hashFS(fsys fs.FS) (string, error) {
	var names []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}
