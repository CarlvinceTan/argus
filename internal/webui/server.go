// Package webui serves the Argus audit console: a local or network-hosted web
// app (Svelte SPA + JSON API) plus the evidence files each run produced.
//
// It replaces the earlier desktop visualiser on purpose: the analyser often runs
// on a different machine than the person reviewing it (the GUI host, a build box,
// a container), and a served app works in all of those cases where a desktop
// bundle does not.
package webui

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed all:dist
var assets embed.FS

// Options configure the server.
type Options struct {
	Addr    string // listen address, e.g. 127.0.0.1:8787 or 0.0.0.0:8787
	Token   string // bearer token required for mutating calls (required off-loopback)
	Roots   []string
	Verbose bool
}

// Server is the HTTP surface of the audit console.
type Server struct {
	service *Service
	options Options
	assets  fs.FS
}

// NewServer prepares the console.
func NewServer(service *Service, options Options) (*Server, error) {
	handler, err := fs.Sub(assets, "dist")
	if err != nil {
		// A build without the frontend (for example during development) still
		// serves the API; the UI is expected from the vite dev server then.
		handler = nil
	}
	if err := checkBindSafety(options); err != nil {
		return nil, err
	}
	return &Server{service: service, options: options, assets: handler}, nil
}

// checkBindSafety refuses to expose a console that can start processes on the
// network without a token.
func checkBindSafety(options Options) error {
	host, _, err := net.SplitHostPort(options.Addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", options.Addr, err)
	}
	if options.Token != "" {
		return nil
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("refusing to listen on %s without --token: the console can start audits (set --token, or bind 127.0.0.1)", options.Addr)
	}
	return nil
}

// Handler returns the HTTP handler for the console.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/roots", s.get(s.handleRoots))
	mux.HandleFunc("/api/runs", s.get(s.handleRuns))
	mux.HandleFunc("/api/run", s.get(s.handleRun))
	mux.HandleFunc("/api/labels", s.get(s.handleLabels))
	mux.HandleFunc("/api/label", s.mutating(s.handleLabel))
	mux.HandleFunc("/api/calibration", s.get(s.handleCalibration))
	mux.HandleFunc("/api/calibration/rebuild", s.mutating(s.handleRebuild))
	mux.HandleFunc("/api/launched", s.get(s.handleLaunched))
	mux.HandleFunc("/api/launch", s.mutating(s.handleLaunch))
	mux.HandleFunc("/api/launch/stop", s.mutating(s.handleLaunchStop))
	mux.HandleFunc("/api/health", s.get(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now()})
	}))
	mux.HandleFunc("/evidence/", s.handleEvidence)
	mux.HandleFunc("/", s.handleStatic)
	return mux
}

// ListenAndServe runs the console until it is stopped.
func (s *Server) ListenAndServe() error {
	server := &http.Server{
		Addr:              s.options.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return server.ListenAndServe()
}

// URL is the address a browser should open.
func (s *Server) URL() string {
	host, port, err := net.SplitHostPort(s.options.Addr)
	if err != nil {
		return "http://" + s.options.Addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		if name, err := os.Hostname(); err == nil {
			return fmt.Sprintf("http://%s:%s (and http://127.0.0.1:%s)", name, port, port)
		}
		return "http://127.0.0.1:" + port
	}
	return "http://" + net.JoinHostPort(host, port)
}

func (s *Server) get(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

// mutating wraps handlers that change state, requiring the token when one is set.
func (s *Server) mutating(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if s.options.Token != "" {
			presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(presented), []byte(s.options.Token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) handleRoots(w http.ResponseWriter, r *http.Request) {
	roots := s.options.Roots
	if len(roots) == 0 {
		roots = s.service.Roots()
	}
	writeJSON(w, http.StatusOK, roots)
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("root")
	if root == "" && len(s.options.Roots) > 0 {
		root = s.options.Roots[0]
	}
	runs, err := s.service.ListRuns(root)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	detail, err := s.service.LoadRun(path)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request) {
	labels, err := s.service.LabelsFor(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, labels)
}

func (s *Server) handleLabel(w http.ResponseWriter, r *http.Request) {
	var request LabelRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, err)
		return
	}
	// The path identifies the run; the id identifies the finding inside it.
	view, err := s.service.Label(request)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleCalibration(w http.ResponseWriter, r *http.Request) {
	view, err := s.service.Calibration()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleRebuild(w http.ResponseWriter, r *http.Request) {
	view, err := s.service.Recalibrate()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleLaunched(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.service.LaunchedRuns())
}

func (s *Server) handleLaunch(w http.ResponseWriter, r *http.Request) {
	var request LaunchRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, err)
		return
	}
	launched, err := s.service.Launch(request)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, launched)
}

func (s *Server) handleLaunchStop(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, err)
		return
	}
	if err := s.service.StopLaunched(request.ID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleEvidence serves stills, strips and clips from inside a run directory.
func (s *Server) handleEvidence(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/evidence/")
	runID, relative, found := strings.Cut(rest, "/")
	if !found {
		http.NotFound(w, r)
		return
	}
	directory, ok := s.service.RunDir(runID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	clean := filepath.Clean("/" + relative)
	target := filepath.Join(directory, clean)
	absolute, err := filepath.Abs(target)
	if err != nil || !strings.HasPrefix(absolute, directory) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if info, err := os.Stat(absolute); err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, absolute)
}

// handleStatic serves the SPA, falling back to index.html for client routes.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if s.assets == nil {
		http.Error(w, "the web UI is not embedded in this build; run the vite dev server and use it directly", http.StatusNotFound)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	if info, err := fs.Stat(s.assets, path); err != nil || info.IsDir() {
		path = "index.html"
	}
	raw, err := fs.ReadFile(s.assets, path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(path, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(path, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(path, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(path, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
}
