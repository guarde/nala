package api

import (
	_ "embed"
	"net/http"
)

//go:embed debug/index.html
var debugHTML []byte

//go:embed debug/app.js
var debugJS []byte

func (s *Server) debugUI(mux *http.ServeMux) {
	serve := func(contentType string, body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
			w.Header().Set("Content-Type", contentType)
			w.Write(body)
		}
	}
	mux.HandleFunc("GET /debug", serve("text/html; charset=utf-8", debugHTML))
	mux.HandleFunc("GET /debug/app.js", serve("text/javascript; charset=utf-8", debugJS))
}
