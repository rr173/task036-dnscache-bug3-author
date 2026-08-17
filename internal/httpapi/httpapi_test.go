package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"task036-dnscache/internal/dnscache"
)

func doRequest(h http.Handler, path string, body any) (int, map[string]any) {
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestPutLookupAndErrors(t *testing.T) {
	h := New(dnscache.New()).Handler()

	code, body := doRequest(h, "/put", map[string]any{"name": "example.com", "type": "A", "ttl": 300, "values": []string{"1.2.3.4"}})
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("put: code=%d body=%v", code, body)
	}
	code, body = doRequest(h, "/lookup", map[string]any{"name": "example.com", "type": "A"})
	if code != http.StatusOK || body["status"] != "NOERROR" {
		t.Fatalf("lookup: code=%d body=%v", code, body)
	}
	vals, _ := body["values"].([]any)
	if len(vals) != 1 || vals[0] != "1.2.3.4" {
		t.Errorf("lookup values=%v want [1.2.3.4]", vals)
	}

	// Error cases return 400.
	cases := []struct {
		name string
		path string
		body any
	}{
		{"bad ipv4", "/put", map[string]any{"name": "x.test", "type": "A", "ttl": 300, "values": []string{"999.1.1.1"}}},
		{"unknown type", "/put", map[string]any{"name": "x.test", "type": "MX", "ttl": 300, "values": []string{"x"}}},
		{"missing ttl", "/put", map[string]any{"name": "x.test", "type": "A", "values": []string{"1.1.1.1"}}},
		{"negative ttl", "/put", map[string]any{"name": "x.test", "type": "A", "ttl": -1, "values": []string{"1.1.1.1"}}},
		{"lookup unknown type", "/lookup", map[string]any{"name": "x.test", "type": "MX"}},
		{"evict unknown type", "/evict", map[string]any{"name": "x.test", "type": "MX"}},
	}
	for _, tc := range cases {
		code, body := doRequest(h, tc.path, tc.body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: code=%d want 400 body=%v", tc.name, code, body)
		}
		if _, ok := body["error"]; !ok {
			t.Errorf("%s: response missing error field: %v", tc.name, body)
		}
	}
}

func TestStatsEndpoint(t *testing.T) {
	h := New(dnscache.New()).Handler()
	_, _ = doRequest(h, "/put", map[string]any{"name": "a.test", "type": "A", "ttl": 300, "values": []string{"1.1.1.1"}})
	code, body := doRequest(h, "/stats", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("stats: code=%d", code)
	}
	if body["record_sets"] != float64(1) {
		t.Errorf("stats record_sets=%v want 1", body["record_sets"])
	}
	byType, _ := body["by_type"].(map[string]any)
	if byType["A"] != float64(1) || byType["AAAA"] != float64(0) || byType["CNAME"] != float64(0) || byType["TXT"] != float64(0) {
		t.Errorf("stats by_type=%v want A=1 others=0", byType)
	}
}

func TestEvictEndpoint(t *testing.T) {
	h := New(dnscache.New()).Handler()
	_, _ = doRequest(h, "/put", map[string]any{"name": "m.test", "type": "A", "ttl": 300, "values": []string{"1.1.1.1"}})
	_, _ = doRequest(h, "/put", map[string]any{"name": "m.test", "type": "AAAA", "ttl": 300, "values": []string{"::1"}})

	code, body := doRequest(h, "/evict", map[string]any{"name": "m.test", "type": "A"})
	if code != http.StatusOK || body["evicted"] != float64(1) {
		t.Fatalf("evict A: code=%d body=%v", code, body)
	}
	_, body = doRequest(h, "/lookup", map[string]any{"name": "m.test", "type": "A"})
	if body["status"] != "MISS" {
		t.Errorf("after evict A, lookup A: status=%v want MISS", body["status"])
	}
	_, body = doRequest(h, "/lookup", map[string]any{"name": "m.test", "type": "AAAA"})
	if body["status"] != "NOERROR" {
		t.Errorf("after evict A, lookup AAAA: status=%v want NOERROR", body["status"])
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := New(dnscache.New()).Handler()
	req := httptest.NewRequest(http.MethodGet, "/lookup", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /lookup: code=%d want 405", w.Code)
	}
}
