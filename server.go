package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

//go:embed web
var webFS embed.FS

const (
	maxRequestBody = 16 << 10
	heartbeatEvery = 15 * time.Second
	eventInterval  = 250 * time.Millisecond
)

type Server struct {
	cfg          Config
	manager      *Manager
	resolve      resolver
	ytDlpVersion atomic.Value
}

func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/meta", s.handleMeta)
	api.HandleFunc("POST /api/probe", s.handleProbe)
	api.HandleFunc("POST /api/downloads", s.handleStart)
	api.HandleFunc("GET /api/downloads/{id}/events", s.handleEvents)
	api.HandleFunc("GET /api/downloads/{id}/file", s.handleFile)
	api.HandleFunc("DELETE /api/downloads/{id}", s.handleCancel)

	static, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	api.Handle("GET /", http.FileServerFS(static))

	protected := http.NewCrossOriginProtection().Handler(s.requireAuth(api))

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	root.Handle("/", protected)
	return securityHeaders(root)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// thumbnails are the only remote content: they live on each site's CDN
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' https:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	if s.cfg.Password == "" {
		return next
	}
	want := sha256.Sum256([]byte(s.cfg.Password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, password, _ := r.BasicAuth()
		got := sha256.Sum256([]byte(password))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Roger", charset="UTF-8"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func readJSON(w http.ResponseWriter, r *http.Request, into any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		return errors.New("Invalid request.")
	}
	return nil
}

func (s *Server) handleMeta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":          version,
		"ytdlp_version":    s.ytDlpVersion.Load(),
		"file_ttl_seconds": int(s.cfg.FileTTL.Seconds()),
	})
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	url, err := checkURL(r.Context(), body.URL, s.cfg.AllowPrivateURLs, s.resolve)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.manager.Probe(r.Context(), url)
	switch {
	case errors.Is(err, context.Canceled):
		return // the browser went away
	case errors.Is(err, errBusy):
		writeError(w, http.StatusTooManyRequests, err)
	case err != nil:
		writeError(w, http.StatusUnprocessableEntity, err)
	default:
		writeJSON(w, http.StatusOK, result)
	}
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProbeID string `json:"probe_id"`
		Choice  int    `json:"choice"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	job, err := s.manager.Start(body.ProbeID, body.Choice)
	switch {
	case errors.Is(err, errBusy):
		writeError(w, http.StatusTooManyRequests, err)
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		status, _ := job.Watch()
		writeJSON(w, http.StatusAccepted, status)
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	job := s.manager.Job(r.PathValue("id"))
	if job == nil {
		writeError(w, http.StatusNotFound, errNotFound)
		return
	}
	defer job.Subscribe()()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx would otherwise hold events back
	flusher := http.NewResponseController(w)

	heartbeat := time.NewTicker(heartbeatEvery)
	defer heartbeat.Stop()
	for {
		status, changed := job.Watch()
		data, err := json.Marshal(status)
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return
		}
		if err := flusher.Flush(); err != nil {
			return
		}
		if status.State.finished() {
			return
		}

	wait:
		for {
			select {
			case <-r.Context().Done():
				return
			case <-changed:
				break wait
			case <-heartbeat.C:
				if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
					return
				}
				if err := flusher.Flush(); err != nil {
					return
				}
			}
		}
		// yt-dlp reports many times a second: updates in between are coalesced
		select {
		case <-r.Context().Done():
			return
		case <-time.After(eventInterval):
		}
	}
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	job := s.manager.Job(r.PathValue("id"))
	if job == nil {
		http.Error(w, "This file has expired.", http.StatusNotFound)
		return
	}
	path, name, ok := job.File()
	if !ok {
		http.Error(w, "This file isn't ready.", http.StatusNotFound)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		http.Error(w, "This file has expired.", http.StatusNotFound)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.Error(w, "This file has expired.", http.StatusNotFound)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	h.Set("Cache-Control", "private, no-store")
	// ServeContent handles Range, so an interrupted save can resume
	http.ServeContent(w, r, "", info.ModTime(), file)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	job := s.manager.Job(r.PathValue("id"))
	if job == nil {
		writeError(w, http.StatusNotFound, errNotFound)
		return
	}
	job.cancel()
	w.WriteHeader(http.StatusNoContent)
}

// keepYtDlpFresh matters on a long-running server: sites change constantly and
// a months-old yt-dlp stops working on them.
func (s *Server) keepYtDlpFresh(ctx context.Context, yt *YtDlp, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		updateCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		out, err := yt.Update(updateCtx)
		if err != nil && ctx.Err() == nil {
			slog.Warn("yt-dlp update failed", "error", err, "output", out)
		} else if v, err := yt.Version(updateCtx); err == nil {
			s.ytDlpVersion.Store(v)
			slog.Info("yt-dlp checked for updates", "version", v)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
