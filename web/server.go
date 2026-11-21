// Package web serves the topology dashboard: a single embedded HTML
// page plus a JSON snapshot API the page polls once a second.
package web

import (
	"embed"
	"encoding/json"
	"net/http"

	"topomgr/manager"
)

//go:embed static/index.html
var staticFS embed.FS

type Server struct {
	mg *manager.Manager
	mx *http.ServeMux
}

func NewServer(mg *manager.Manager) *Server {
	s := &Server{mg: mg, mx: http.NewServeMux()}
	s.mx.HandleFunc("/", s.index)
	s.mx.HandleFunc("/api/state", s.state)
	return s
}

func (s *Server) Handler() http.Handler { return s.mx }

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.mg.Snapshot())
}
