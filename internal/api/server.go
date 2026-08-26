package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/trucore-ai/meshdns/internal/config"
	"github.com/trucore-ai/meshdns/internal/store"
)

var logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// Server holds the HTTP server dependencies.
type Server struct {
	Store  *store.Store
	Config *config.Config
	mux    *http.ServeMux
}

// NewServer creates a new API server with all routes mounted.
func NewServer(s *store.Store, cfg *config.Config) *Server {
	srv := &Server{Store: s, Config: cfg, mux: http.NewServeMux()}
	srv.mountRoutes()
	return srv
}

func (s *Server) mountRoutes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /llms.txt", s.handleLLMsTxt)
	s.mux.HandleFunc("GET /", s.handleLanding)
	s.mux.HandleFunc("POST /v0/servers", s.handleRegister)
	s.mux.HandleFunc("GET /v0/servers", s.handleListServers)
	s.mux.HandleFunc("GET /v0/servers/{id}", s.handleGetServer)
	s.mux.HandleFunc("PUT /v0/servers/{id}", s.handleUpdateServer)
	s.mux.HandleFunc("DELETE /v0/servers/{id}", s.handleDelistServer)
	s.mux.HandleFunc("GET /v0/resolve", s.handleResolve)
	s.mux.HandleFunc("GET /v0/export", s.handleExport)
	s.mux.HandleFunc("GET /v0/stats", s.handleStats)
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Write-Key")

	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}

	// Log every request
	logger.Info("request",
		"method", r.Method,
		"path", r.URL.Path,
		"remote", r.RemoteAddr,
	)

	s.mux.ServeHTTP(w, r)
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":   code,
			"detail": detail,
		},
	})
}

// extractWriteKey extracts the server write key from the X-Write-Key header
// or Authorization: Bearer header. Returns the raw key (not the hash).
func extractWriteKey(r *http.Request) string {
	if key := r.Header.Get("X-Write-Key"); key != "" {
		return key
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}