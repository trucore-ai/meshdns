package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/trucore-ai/meshdns/internal/store"
)

type Server struct {
	store *store.Store
}

func New(st *store.Store) *Server {
	return &Server{store: st}
}

func (s *Server) Router() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /v0/servers", s.handleListServers)
	mux.HandleFunc("POST /v0/servers", s.handleRegisterServer)
	mux.HandleFunc("PUT /v0/servers/{id}", s.handleUpdateServer)
	mux.HandleFunc("DELETE /v0/servers/{id}", s.handleDeleteServer)
	mux.HandleFunc("GET /v0/resolve", s.handleResolve)
	mux.HandleFunc("GET /v0/export", s.handleExport)
	mux.HandleFunc("GET /v0/stats", s.handleStats)

	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string, detail any) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":   code,
			"detail": detail,
		},
	})
}

func uuidV4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func randomHexToken(byteCount int) (string, error) {
	b := make([]byte, byteCount)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}
