// Package selfcheck runs an end-to-end verification of the dnscache service
// against an in-process HTTP server backed by a controllable clock. It is
// invoked by the --smoke-test flag and exits the process on completion.
//
// Because the cache is global mutable state, each scenario builds its own
// fresh cache+server+clock so state never leaks between scenarios.
package selfcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"task036-dnscache/internal/dnscache"
	"task036-dnscache/internal/httpapi"
)

// clock is a controllable time source for deterministic TTL tests.
type clock struct {
	t time.Time
}

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// env bundles a fresh server, client, and clock for one scenario.
type env struct {
	base string
	c    *http.Client
	clk  *clock
	srv  *httptest.Server
}

func newEnv() *env {
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	cache := dnscache.NewWithClock(clk.now)
	srv := httptest.NewServer(httpapi.New(cache).Handler())
	return &env{
		base: srv.URL,
		c:    srv.Client(),
		clk:  clk,
		srv:  srv,
	}
}

func (e *env) close() { e.srv.Close() }

// Run exercises the full HTTP API across isolated scenarios, returning nil if
// every behavior matches the specification. On failure it returns an error
// describing the first mismatch.
func Run() error {
	scenarios := []struct {
		name string
		fn   func(*env) error
	}{
		{"put+lookup fresh", scenarioPutLookup},
		{"ttl expiry and remaining", scenarioTTLExpiry},
		{"ttl=0 never served", scenarioTTLZero},
		{"cname chain resolution", scenarioCNAMEChain},
		{"cname loop", scenarioCNAMELoop},
		{"cname too long (11 hops)", scenarioCNAMETooLong},
		{"cname exact limit (10 hops)", scenarioCNAMEExactLimit},
		{"nxdomain negative cache", scenarioNXDOMAIN},
		{"nxdomain cleared by put", scenarioNXDOMAINPut},
		{"nxdomain cleared by evict", scenarioNXDOMAINEvict},
		{"case-insensitive names", scenarioCaseInsensitive},
		{"evict by type and by name", scenarioEvict},
		{"replace on put", scenarioReplace},
		{"txt record", scenarioTXT},
		{"cname lookup returns cname", scenarioCNAMELookup},
		{"stats counts and expired", scenarioStats},
		{"error cases", scenarioErrors},
	}
	for _, sc := range scenarios {
		e := newEnv()
		err := sc.fn(e)
		e.close()
		if err != nil {
			return fmt.Errorf("%s: %w", sc.name, err)
		}
	}
	return nil
}

// ---- HTTP helper ----

func post(e *env, path string, body any) (int, map[string]any, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, e.base+path, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &out)
	}
	return resp.StatusCode, out, nil
}

// ---- scenarios ----

func scenarioPutLookup(e *env) error {
	if code, body, err := post(e, "/put", map[string]any{
		"name": "example.com", "type": "A", "ttl": 300, "values": []string{"1.2.3.4", "5.6.7.8"},
	}); err != nil {
		return err
	} else if code != http.StatusOK || body["ok"] != true {
		return fmt.Errorf("put: code=%d body=%v", code, body)
	}
	if code, body, err := post(e, "/lookup", map[string]any{"name": "example.com", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "NOERROR" {
		return fmt.Errorf("lookup: code=%d body=%v", code, body)
	} else {
		vals, _ := body["values"].([]any)
		if len(vals) != 2 || vals[0] != "1.2.3.4" || vals[1] != "5.6.7.8" {
			return fmt.Errorf("lookup values: %v want [1.2.3.4 5.6.7.8]", vals)
		}
		if body["ttl"] != float64(300) {
			return fmt.Errorf("lookup ttl: %v want 300", body["ttl"])
		}
		if body["ttl_remaining"] != float64(300) {
			return fmt.Errorf("lookup ttl_remaining: %v want 300", body["ttl_remaining"])
		}
	}
	return nil
}

func scenarioTTLExpiry(e *env) error {
	if _, _, err := post(e, "/put", map[string]any{"name": "a.test", "type": "A", "ttl": 300, "values": []string{"1.1.1.1"}}); err != nil {
		return err
	}
	e.clk.advance(299 * time.Second)
	if code, body, err := post(e, "/lookup", map[string]any{"name": "a.test", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "NOERROR" {
		return fmt.Errorf("at 299s: code=%d status=%v want NOERROR", code, body["status"])
	} else if body["ttl_remaining"] != float64(1) {
		return fmt.Errorf("at 299s: ttl_remaining=%v want 1", body["ttl_remaining"])
	}
	e.clk.advance(1 * time.Second) // total 300s -> expired
	if code, body, err := post(e, "/lookup", map[string]any{"name": "a.test", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "MISS" {
		return fmt.Errorf("at 300s: code=%d status=%v want MISS", code, body["status"])
	}
	// Lazy eviction: the expired record set is gone from stats.
	if code, body, err := post(e, "/stats", map[string]any{}); err != nil {
		return err
	} else if code != http.StatusOK || body["record_sets"] != float64(0) {
		return fmt.Errorf("stats after expiry: record_sets=%v want 0", body["record_sets"])
	}
	return nil
}

func scenarioTTLZero(e *env) error {
	if _, _, err := post(e, "/put", map[string]any{"name": "z.test", "type": "A", "ttl": 0, "values": []string{"1.1.1.1"}}); err != nil {
		return err
	}
	if code, body, err := post(e, "/lookup", map[string]any{"name": "z.test", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "MISS" {
		return fmt.Errorf("ttl=0 lookup: code=%d status=%v want MISS", code, body["status"])
	}
	return nil
}

func scenarioCNAMEChain(e *env) error {
	if _, _, err := post(e, "/put", map[string]any{"name": "alias.test", "type": "CNAME", "ttl": 600, "values": []string{"target.test"}}); err != nil {
		return err
	}
	if _, _, err := post(e, "/put", map[string]any{"name": "target.test", "type": "A", "ttl": 600, "values": []string{"9.9.9.9"}}); err != nil {
		return err
	}
	if code, body, err := post(e, "/lookup", map[string]any{"name": "alias.test", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "NOERROR" {
		return fmt.Errorf("cname chain: code=%d status=%v want NOERROR", code, body["status"])
	} else {
		vals, _ := body["values"].([]any)
		if len(vals) != 1 || vals[0] != "9.9.9.9" {
			return fmt.Errorf("cname chain values: %v want [9.9.9.9]", vals)
		}
	}
	return nil
}

func scenarioCNAMELoop(e *env) error {
	if _, _, err := post(e, "/put", map[string]any{"name": "a.test", "type": "CNAME", "ttl": 600, "values": []string{"b.test"}}); err != nil {
		return err
	}
	if _, _, err := post(e, "/put", map[string]any{"name": "b.test", "type": "CNAME", "ttl": 600, "values": []string{"a.test"}}); err != nil {
		return err
	}
	if code, body, err := post(e, "/lookup", map[string]any{"name": "a.test", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "CNAME_LOOP" {
		return fmt.Errorf("cname loop: code=%d status=%v want CNAME_LOOP", code, body["status"])
	}
	return nil
}

func scenarioCNAMETooLong(e *env) error {
	// 11 CNAMEs: c1->c2->...->c12, A on c12. The 11th follow yields TOO_LONG.
	for i := 1; i <= 11; i++ {
		if _, _, err := post(e, "/put", map[string]any{
			"name": fmt.Sprintf("c%d.test", i), "type": "CNAME", "ttl": 600,
			"values": []string{fmt.Sprintf("c%d.test", i+1)},
		}); err != nil {
			return err
		}
	}
	if _, _, err := post(e, "/put", map[string]any{"name": "c12.test", "type": "A", "ttl": 600, "values": []string{"1.1.1.1"}}); err != nil {
		return err
	}
	if code, body, err := post(e, "/lookup", map[string]any{"name": "c1.test", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "CNAME_TOO_LONG" {
		return fmt.Errorf("cname too long: code=%d status=%v want CNAME_TOO_LONG", code, body["status"])
	}
	return nil
}

func scenarioCNAMEExactLimit(e *env) error {
	// 10 CNAMEs: c1->...->c11, A on c11. Exactly 10 follows -> NOERROR.
	for i := 1; i <= 10; i++ {
		if _, _, err := post(e, "/put", map[string]any{
			"name": fmt.Sprintf("c%d.test", i), "type": "CNAME", "ttl": 600,
			"values": []string{fmt.Sprintf("c%d.test", i+1)},
		}); err != nil {
			return err
		}
	}
	if _, _, err := post(e, "/put", map[string]any{"name": "c11.test", "type": "A", "ttl": 600, "values": []string{"1.1.1.1"}}); err != nil {
		return err
	}
	if code, body, err := post(e, "/lookup", map[string]any{"name": "c1.test", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "NOERROR" {
		return fmt.Errorf("cname exact limit: code=%d status=%v want NOERROR", code, body["status"])
	}
	return nil
}

func scenarioNXDOMAIN(e *env) error {
	if _, _, err := post(e, "/mark-nxdomain", map[string]any{"name": "ghost.test", "ttl": 60}); err != nil {
		return err
	}
	if code, body, err := post(e, "/lookup", map[string]any{"name": "ghost.test", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "NXDOMAIN" {
		return fmt.Errorf("nxdomain fresh: code=%d status=%v want NXDOMAIN", code, body["status"])
	}
	// NXDOMAIN applies to any type.
	if code, body, err := post(e, "/lookup", map[string]any{"name": "ghost.test", "type": "TXT"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "NXDOMAIN" {
		return fmt.Errorf("nxdomain txt: code=%d status=%v want NXDOMAIN", code, body["status"])
	}
	e.clk.advance(59 * time.Second)
	if code, body, err := post(e, "/lookup", map[string]any{"name": "ghost.test", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "NXDOMAIN" {
		return fmt.Errorf("nxdomain at 59s: code=%d status=%v want NXDOMAIN", code, body["status"])
	}
	e.clk.advance(1 * time.Second) // total 60s -> expired
	if code, body, err := post(e, "/lookup", map[string]any{"name": "ghost.test", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "MISS" {
		return fmt.Errorf("nxdomain at 60s: code=%d status=%v want MISS", code, body["status"])
	}
	return nil
}

func scenarioNXDOMAINPut(e *env) error {
	if _, _, err := post(e, "/mark-nxdomain", map[string]any{"name": "ghost.test", "ttl": 60}); err != nil {
		return err
	}
	if _, _, err := post(e, "/put", map[string]any{"name": "ghost.test", "type": "A", "ttl": 300, "values": []string{"1.1.1.1"}}); err != nil {
		return err
	}
	if code, body, err := post(e, "/lookup", map[string]any{"name": "ghost.test", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "NOERROR" {
		return fmt.Errorf("after put: code=%d status=%v want NOERROR", code, body["status"])
	}
	return nil
}

func scenarioNXDOMAINEvict(e *env) error {
	if _, _, err := post(e, "/mark-nxdomain", map[string]any{"name": "ghost.test", "ttl": 60}); err != nil {
		return err
	}
	if code, body, err := post(e, "/evict", map[string]any{"name": "ghost.test"}); err != nil {
		return err
	} else if code != http.StatusOK || body["negative_cleared"] != true {
		return fmt.Errorf("evict nxdomain: code=%d body=%v", code, body)
	}
	if code, body, err := post(e, "/lookup", map[string]any{"name": "ghost.test", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "MISS" {
		return fmt.Errorf("after evict: code=%d status=%v want MISS", code, body["status"])
	}
	return nil
}

func scenarioCaseInsensitive(e *env) error {
	if _, _, err := post(e, "/put", map[string]any{"name": "Example.COM", "type": "A", "ttl": 300, "values": []string{"1.1.1.1"}}); err != nil {
		return err
	}
	for _, q := range []string{"example.com", "EXAMPLE.COM", "ExAmPlE.CoM"} {
		if code, body, err := post(e, "/lookup", map[string]any{"name": q, "type": "A"}); err != nil {
			return err
		} else if code != http.StatusOK || body["status"] != "NOERROR" {
			return fmt.Errorf("lookup %q: code=%d status=%v want NOERROR", q, code, body["status"])
		}
	}
	return nil
}

func scenarioEvict(e *env) error {
	if _, _, err := post(e, "/put", map[string]any{"name": "m.test", "type": "A", "ttl": 300, "values": []string{"1.1.1.1"}}); err != nil {
		return err
	}
	if _, _, err := post(e, "/put", map[string]any{"name": "m.test", "type": "AAAA", "ttl": 300, "values": []string{"::1"}}); err != nil {
		return err
	}
	// Evict by type A only.
	if code, body, err := post(e, "/evict", map[string]any{"name": "m.test", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["evicted"] != float64(1) {
		return fmt.Errorf("evict A: code=%d body=%v", code, body)
	}
	if code, body, err := post(e, "/lookup", map[string]any{"name": "m.test", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "MISS" {
		return fmt.Errorf("after evict A, lookup A: status=%v want MISS", body["status"])
	}
	if code, body, err := post(e, "/lookup", map[string]any{"name": "m.test", "type": "AAAA"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "NOERROR" {
		return fmt.Errorf("after evict A, lookup AAAA: status=%v want NOERROR", body["status"])
	}
	// Evict all by name: only AAAA remains, so evicted=1.
	if code, body, err := post(e, "/evict", map[string]any{"name": "m.test"}); err != nil {
		return err
	} else if code != http.StatusOK || body["evicted"] != float64(1) {
		return fmt.Errorf("evict all: code=%d body=%v (want evicted=1)", code, body)
	}
	if code, body, err := post(e, "/lookup", map[string]any{"name": "m.test", "type": "AAAA"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "MISS" {
		return fmt.Errorf("after evict all, lookup AAAA: status=%v want MISS", body["status"])
	}
	return nil
}

func scenarioReplace(e *env) error {
	if _, _, err := post(e, "/put", map[string]any{"name": "r.test", "type": "A", "ttl": 300, "values": []string{"1.1.1.1"}}); err != nil {
		return err
	}
	if _, _, err := post(e, "/put", map[string]any{"name": "r.test", "type": "A", "ttl": 300, "values": []string{"2.2.2.2", "3.3.3.3"}}); err != nil {
		return err
	}
	if code, body, err := post(e, "/lookup", map[string]any{"name": "r.test", "type": "A"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "NOERROR" {
		return fmt.Errorf("replace lookup: code=%d status=%v", code, body["status"])
	} else {
		vals, _ := body["values"].([]any)
		if len(vals) != 2 || vals[0] != "2.2.2.2" || vals[1] != "3.3.3.3" {
			return fmt.Errorf("replace values: %v want [2.2.2.2 3.3.3.3]", vals)
		}
	}
	// Replaced, not merged: only one record set.
	if code, body, err := post(e, "/stats", map[string]any{}); err != nil {
		return err
	} else if code != http.StatusOK || body["record_sets"] != float64(1) {
		return fmt.Errorf("replace stats: record_sets=%v want 1", body["record_sets"])
	}
	return nil
}

func scenarioTXT(e *env) error {
	if _, _, err := post(e, "/put", map[string]any{"name": "t.test", "type": "TXT", "ttl": 300, "values": []string{"v=spf1 -all", "hello"}}); err != nil {
		return err
	}
	if code, body, err := post(e, "/lookup", map[string]any{"name": "t.test", "type": "TXT"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "NOERROR" {
		return fmt.Errorf("txt lookup: code=%d status=%v", code, body["status"])
	} else {
		vals, _ := body["values"].([]any)
		if len(vals) != 2 || vals[0] != "v=spf1 -all" || vals[1] != "hello" {
			return fmt.Errorf("txt values: %v want [v=spf1 -all hello]", vals)
		}
	}
	return nil
}

func scenarioCNAMELookup(e *env) error {
	if _, _, err := post(e, "/put", map[string]any{"name": "alias.test", "type": "CNAME", "ttl": 600, "values": []string{"target.test"}}); err != nil {
		return err
	}
	// A CNAME lookup returns the CNAME itself, no following.
	if code, body, err := post(e, "/lookup", map[string]any{"name": "alias.test", "type": "CNAME"}); err != nil {
		return err
	} else if code != http.StatusOK || body["status"] != "NOERROR" {
		return fmt.Errorf("cname lookup: code=%d status=%v want NOERROR", code, body["status"])
	} else {
		vals, _ := body["values"].([]any)
		if len(vals) != 1 || vals[0] != "target.test" {
			return fmt.Errorf("cname lookup values: %v want [target.test]", vals)
		}
	}
	return nil
}

func scenarioStats(e *env) error {
	if _, _, err := post(e, "/put", map[string]any{"name": "a.test", "type": "A", "ttl": 300, "values": []string{"1.1.1.1"}}); err != nil {
		return err
	}
	if _, _, err := post(e, "/put", map[string]any{"name": "b.test", "type": "A", "ttl": 300, "values": []string{"2.2.2.2"}}); err != nil {
		return err
	}
	if _, _, err := post(e, "/put", map[string]any{"name": "c.test", "type": "AAAA", "ttl": 10, "values": []string{"::1"}}); err != nil {
		return err
	}
	if _, _, err := post(e, "/put", map[string]any{"name": "d.test", "type": "CNAME", "ttl": 300, "values": []string{"a.test"}}); err != nil {
		return err
	}
	if _, _, err := post(e, "/mark-nxdomain", map[string]any{"name": "ghost.test", "ttl": 60}); err != nil {
		return err
	}
	// Advance past c.test's short TTL (10s): one record set now expired but still stored.
	e.clk.advance(20 * time.Second)
	if code, body, err := post(e, "/stats", map[string]any{}); err != nil {
		return err
	} else if code != http.StatusOK {
		return fmt.Errorf("stats: code=%d", code)
	} else {
		// 4 record sets (a, b, c, d); c expired but still in memory.
		if body["record_sets"] != float64(4) {
			return fmt.Errorf("stats record_sets=%v want 4", body["record_sets"])
		}
		byType, _ := body["by_type"].(map[string]any)
		if byType["A"] != float64(2) || byType["AAAA"] != float64(1) || byType["CNAME"] != float64(1) || byType["TXT"] != float64(0) {
			return fmt.Errorf("stats by_type=%v want A=2 AAAA=1 CNAME=1 TXT=0", byType)
		}
		if body["negative"] != float64(1) {
			return fmt.Errorf("stats negative=%v want 1", body["negative"])
		}
		if body["expired"] != float64(1) { // only c.test (AAAA); negative still fresh at 20s<60s
			return fmt.Errorf("stats expired=%v want 1", body["expired"])
		}
	}
	return nil
}

func scenarioErrors(e *env) error {
	cases := []struct {
		name string
		path string
		body any
	}{
		{"bad ipv4", "/put", map[string]any{"name": "x.test", "type": "A", "ttl": 300, "values": []string{"999.999.999.999"}}},
		{"bad ipv6", "/put", map[string]any{"name": "x.test", "type": "AAAA", "ttl": 300, "values": []string{"not-an-ipv6"}}},
		{"ipv4 as aaaa", "/put", map[string]any{"name": "x.test", "type": "AAAA", "ttl": 300, "values": []string{"1.2.3.4"}}},
		{"cname two values", "/put", map[string]any{"name": "x.test", "type": "CNAME", "ttl": 300, "values": []string{"a.test", "b.test"}}},
		{"unknown type put", "/put", map[string]any{"name": "x.test", "type": "MX", "ttl": 300, "values": []string{"x"}}},
		{"negative ttl", "/put", map[string]any{"name": "x.test", "type": "A", "ttl": -1, "values": []string{"1.1.1.1"}}},
		{"empty name", "/put", map[string]any{"name": "", "type": "A", "ttl": 300, "values": []string{"1.1.1.1"}}},
		{"bad name chars", "/put", map[string]any{"name": "bad name!", "type": "A", "ttl": 300, "values": []string{"1.1.1.1"}}},
		{"missing ttl", "/put", map[string]any{"name": "x.test", "type": "A", "values": []string{"1.1.1.1"}}},
		{"empty values", "/put", map[string]any{"name": "x.test", "type": "A", "ttl": 300, "values": []string{}}},
		{"txt too long", "/put", map[string]any{"name": "x.test", "type": "TXT", "ttl": 300, "values": []string{strings.Repeat("a", 256)}}},
		{"lookup unknown type", "/lookup", map[string]any{"name": "x.test", "type": "MX"}},
		{"nxdomain negative ttl", "/mark-nxdomain", map[string]any{"name": "x.test", "ttl": -1}},
		{"evict unknown type", "/evict", map[string]any{"name": "x.test", "type": "MX"}},
	}
	for _, tc := range cases {
		if code, body, err := post(e, tc.path, tc.body); err != nil {
			return fmt.Errorf("%s: %w", tc.name, err)
		} else if code != http.StatusBadRequest {
			return fmt.Errorf("%s: code=%d want 400 body=%v", tc.name, code, body)
		} else if _, ok := body["error"]; !ok {
			return fmt.Errorf("%s: response missing error field: %v", tc.name, body)
		}
	}
	// Error cases did not mutate the cache: a valid put still succeeds.
	if code, _, err := post(e, "/put", map[string]any{"name": "ok.test", "type": "A", "ttl": 300, "values": []string{"1.1.1.1"}}); err != nil {
		return err
	} else if code != http.StatusOK {
		return fmt.Errorf("valid put after errors: code=%d want 200", code)
	}
	return nil
}
