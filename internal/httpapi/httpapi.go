// Package httpapi exposes the dnscache service over HTTP+JSON.
package httpapi

import (
	"encoding/json"
	"net/http"

	"task036-dnscache/internal/dnscache"
)

// Server serves the dnscache API backed by a single mutable cache.
type Server struct {
	cache *dnscache.Cache
}

// New returns a Server for the given cache.
func New(cache *dnscache.Cache) *Server {
	return &Server{cache: cache}
}

// Handler returns the HTTP handler serving the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /put", s.handlePut)
	mux.HandleFunc("POST /lookup", s.handleLookup)
	mux.HandleFunc("POST /mark-nxdomain", s.handleMarkNXDOMAIN)
	mux.HandleFunc("POST /evict", s.handleEvict)
	mux.HandleFunc("POST /stats", s.handleStats)
	return mux
}

// ---- request / response types ----

type putReq struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	TTL    *int     `json:"ttl"` // pointer so a missing field is distinguishable from 0
	Values []string `json:"values"`
}

type nxdomainReq struct {
	Name string `json:"name"`
	TTL  *int   `json:"ttl"`
}

type lookupReq struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type evictReq struct {
	Name string `json:"name"`
	Type string `json:"type"` // optional; empty => all types
}

type errResp struct {
	Error string `json:"error"`
}

// ---- handlers ----

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	var req putReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	t, ok := dnscache.ParseType(req.Type)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unsupported type")
		return
	}
	if req.TTL == nil {
		writeErr(w, http.StatusBadRequest, "missing ttl")
		return
	}
	if err := s.cache.Put(req.Name, t, *req.TTL, req.Values); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMarkNXDOMAIN(w http.ResponseWriter, r *http.Request) {
	var req nxdomainReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TTL == nil {
		writeErr(w, http.StatusBadRequest, "missing ttl")
		return
	}
	if err := s.cache.MarkNXDOMAIN(req.Name, *req.TTL); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLookup(w http.ResponseWriter, r *http.Request) {
	var req lookupReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	t, ok := dnscache.ParseType(req.Type)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unsupported type")
		return
	}
	res := s.cache.Lookup(req.Name, t)
	resp := map[string]any{
		"status": string(res.Status),
		"name":   res.Name,
		"type":   string(res.Type),
	}
	if res.Status == dnscache.StatusNOERROR {
		resp["values"] = res.Values
		resp["ttl"] = res.TTL
		resp["ttl_remaining"] = res.TTLRemaining
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleEvict(w http.ResponseWriter, r *http.Request) {
	var req evictReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var t dnscache.Type
	if req.Type != "" {
		var ok bool
		t, ok = dnscache.ParseType(req.Type)
		if !ok {
			writeErr(w, http.StatusBadRequest, "unsupported type")
			return
		}
	}
	evicted, negCleared := s.cache.Evict(req.Name, t)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"evicted":          evicted,
		"negative_cleared": negCleared,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	st := s.cache.Stats()
	byType := make(map[string]int, len(st.ByType))
	for t, n := range st.ByType {
		byType[string(t)] = n
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"record_sets": st.RecordSets,
		"by_type":     byType,
		"negative":    st.Negative,
		"expired":     st.Expired,
	})
}

// ---- helpers ----

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	defer r.Body.Close()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errResp{Error: msg})
}
