// Package server is the stt-router HTTP server: client API, routing,
// account state, management API and dashboard.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jamir-boop/stt-router/internal/config"
)

//go:embed dashboard.html
var dashboardHTML []byte

// Options configure a Server.
type Options struct {
	ConfigPath string           // where dashboard changes are saved
	StatePath  string           // state file; empty uses the config value or the default
	Listen     string           // effective listen address; empty uses the config value
	Version    string           // shown in the dashboard and the User-Agent
	Now        func() time.Time // clock, for tests
	TempDir    string           // spool directory; empty uses os.TempDir()
	Logger     *log.Logger      // request log lines; nil uses the standard logger
}

// Server routes transcription requests across provider accounts.
type Server struct {
	opts      Options
	statePath string
	cfg       atomic.Pointer[config.Config]
	cfgMu     sync.Mutex // serializes config edits

	mu         sync.Mutex // guards the fields below; never held during an upstream call
	accounts   map[string]*account
	clocks     map[string]float64
	transports map[string]*http.Transport
	reqLog     []LogEntry
	totals     Totals

	rtMu     sync.Mutex
	rts      map[*rtConn]struct{} // open realtime connections, so Close can end them
	rtClosed bool
	rtWG     sync.WaitGroup // realtime handlers, so Close waits for their goroutines

	saveMu sync.Mutex
	dirty  chan struct{}
	stop   chan struct{}
	done   chan struct{}
	closed sync.Once
}

// New creates a server and loads the state file.
func New(cfg *config.Config, opts Options) (*Server, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Listen == "" {
		opts.Listen = cfg.Listen
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	s := &Server{
		opts:       opts,
		statePath:  opts.StatePath,
		accounts:   map[string]*account{},
		clocks:     map[string]float64{},
		transports: map[string]*http.Transport{},
		rts:        map[*rtConn]struct{}{},
		dirty:      make(chan struct{}, 1),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	if s.statePath == "" {
		s.statePath = cfg.StateFile
	}
	if s.statePath == "" {
		s.statePath = config.DefaultStatePath()
	}
	s.cfg.Store(cfg)
	if err := s.loadState(); err != nil {
		return nil, err
	}
	go s.persistLoop()
	return s, nil
}

// Close flushes the state file and frees idle connections. It also closes the
// open realtime connections, which an http.Server.Shutdown does not touch.
func (s *Server) Close() error {
	var err error
	s.closed.Do(func() {
		close(s.stop)
		s.closeRealtime()
		s.rtWG.Wait()
		<-s.done
		err = s.saveState()
		s.mu.Lock()
		for _, t := range s.transports {
			t.CloseIdleConnections()
		}
		s.mu.Unlock()
	})
	return err
}

// Handler returns the HTTP handler with all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.opts.Version})
	})
	mux.HandleFunc("POST /v1/audio/transcriptions", s.handleTranscribe)
	mux.HandleFunc("GET /v1/realtime", s.handleRealtime)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		ancestors := "'none'"
		if fa := s.cfg.Load().FrameAncestors; len(fa) > 0 {
			ancestors = strings.Join(fa, " ")
		}
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors "+ancestors)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Write(dashboardHTML)
	})
	s.mgmtRoutes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "no route for "+r.Method+" "+r.URL.Path)
	})
	return mux
}

func (s *Server) now() time.Time { return s.opts.Now() }

// CloseRealtime closes every open realtime connection. The process shutdown
// calls it through Close; tests can call it to end a connection on purpose.
func (s *Server) CloseRealtime() { s.closeRealtime() }

// logLine writes one finished request to the log. Both the batch handler and
// a realtime turn call it, so their lines look the same.
func (s *Server) logLine(e LogEntry) {
	s.opts.Logger.Printf("transcribe model=%q provider=%q account=%q status=%d attempts=%d latency=%dms bytes=%d",
		e.Model, e.Provider, e.Account, e.Status, e.Attempts, e.LatencyMs, e.UploadBytes)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	if _, ok := s.clientAuth(cfg, r); !ok {
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "missing or invalid client key")
		return
	}
	names := make([]string, 0, len(cfg.Models))
	for m := range cfg.Models {
		names = append(names, m)
	}
	sort.Strings(names)
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]model, 0, len(names))
	for _, n := range names {
		data = append(data, model{ID: n, Object: "model", OwnedBy: "stt-router"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// clientAuth checks the client key and returns its name.
func (s *Server) clientAuth(cfg *config.Config, r *http.Request) (string, bool) {
	tok := bearer(r)
	if len(cfg.ClientKeys) == 0 && config.IsLoopback(s.opts.Listen) {
		return "", true
	}
	if tok == "" {
		return "", false
	}
	name, found := "", false
	for _, k := range cfg.ClientKeys {
		// Every key is compared, so the time does not show which one matched.
		if keyEqual(tok, config.Resolve(k.Key)) && !found {
			name, found = k.Name, true
		}
	}
	return name, found
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// keyEqual compares in constant time; hashing first hides the key length.
func keyEqual(a, b string) bool {
	if b == "" {
		return false
	}
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// apiError is the OpenAI error JSON shape.
type apiError struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    string  `json:"code,omitempty"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	var e apiError
	e.Error.Message, e.Error.Type, e.Error.Code = msg, typ, code
	writeJSON(w, status, e)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// MaskKey shows only the first and last 4 characters of a key.
// "env:NAME" references are not secret and are shown as they are.
func MaskKey(k string) string {
	if strings.HasPrefix(k, "env:") {
		return k
	}
	if len(k) < 12 {
		return "****"
	}
	return k[:4] + "..." + k[len(k)-4:]
}

// RandomKey returns a new random key with a prefix.
func RandomKey(prefix string) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// EnsureStartup applies the startup rules for keys. On a loopback address
// without a management key it generates one, saves it and returns it so the
// caller can print it once. On any other address it refuses to start without
// client keys and a management key.
func EnsureStartup(path string, cfg *config.Config, listen string) (string, error) {
	if !config.IsLoopback(listen) {
		if len(cfg.ClientKeys) == 0 || cfg.ManagementKey == "" {
			return "", errors.New("listen address " + listen + " is not loopback: set client_keys and management_key in the config first")
		}
		return "", nil
	}
	if cfg.ManagementKey != "" {
		return "", nil
	}
	cfg.ManagementKey = RandomKey("sr-mgmt-")
	if err := config.Save(path, cfg); err != nil {
		return "", fmt.Errorf("save generated management key: %w", err)
	}
	return cfg.ManagementKey, nil
}
