package dnscache

import (
	"fmt"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time     { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func testCache() (*Cache, *fakeClock) {
	cl := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	return NewWithClock(cl.now), cl
}

func TestNormalizeName(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"example.com", "example.com", false},
		{"Example.COM", "example.com", false},
		{"a.b.c.test", "a.b.c.test", false},
		{"a-b_c.test", "a-b_c.test", false},
		{"", "", true},
		{"bad name!", "", true},
		{"bad/slash", "", true},
		{"example.com.", "", true},
	}
	for _, tc := range cases {
		got, err := NormalizeName(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NormalizeName(%q): want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeName(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeName(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseType(t *testing.T) {
	cases := []struct {
		in     string
		want   Type
		wantOk bool
	}{
		{"A", TypeA, true},
		{"a", TypeA, true},
		{"aaaa", TypeAAAA, true},
		{"Cname", TypeCNAME, true},
		{"TXT", TypeTXT, true},
		{"MX", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := ParseType(tc.in)
		if got != tc.want || ok != tc.wantOk {
			t.Errorf("ParseType(%q): got %q,%v want %q,%v", tc.in, got, ok, tc.want, tc.wantOk)
		}
	}
}

func TestValidateValues(t *testing.T) {
	cases := []struct {
		name    string
		typ     Type
		values  []string
		wantErr bool
	}{
		{"valid A", TypeA, []string{"1.2.3.4", "10.0.0.1"}, false},
		{"invalid A ipv6", TypeA, []string{"::1"}, true},
		{"invalid A garbage", TypeA, []string{"999.1.1.1"}, true},
		{"valid AAAA", TypeAAAA, []string{"::1", "2001:db8::1"}, false},
		{"invalid AAAA ipv4", TypeAAAA, []string{"1.2.3.4"}, true},
		{"valid CNAME", TypeCNAME, []string{"Target.TEST"}, false},
		{"CNAME two values", TypeCNAME, []string{"a.test", "b.test"}, true},
		{"CNAME empty", TypeCNAME, []string{}, true},
		{"valid TXT", TypeTXT, []string{"hello", "v=spf1 -all"}, false},
		{"TXT empty string", TypeTXT, []string{""}, true},
		{"TXT too long", TypeTXT, []string{strings_Repeat(256)}, true},
		{"empty values", TypeA, []string{}, true},
	}
	for _, tc := range cases {
		_, err := ValidateValues(tc.typ, tc.values)
		if tc.wantErr && err == nil {
			t.Errorf("%s: want error", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
	}
	// CNAME target is normalized to lowercase.
	vals, err := ValidateValues(TypeCNAME, []string{"Target.TEST"})
	if err != nil {
		t.Fatalf("cname normalize: %v", err)
	}
	if vals[0] != "target.test" {
		t.Errorf("cname normalize: got %q want target.test", vals[0])
	}
}

// strings_Repeat avoids importing strings just for one call in a table test.
func strings_Repeat(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func TestPutLookupFresh(t *testing.T) {
	c, cl := testCache()
	if err := c.Put("example.com", TypeA, 300, []string{"1.1.1.1", "2.2.2.2"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	res := c.Lookup("example.com", TypeA)
	if res.Status != StatusNOERROR {
		t.Fatalf("lookup: status=%v want NOERROR", res.Status)
	}
	if len(res.Values) != 2 || res.Values[0] != "1.1.1.1" || res.Values[1] != "2.2.2.2" {
		t.Errorf("lookup values=%v", res.Values)
	}
	if res.TTL != 300 || res.TTLRemaining != 300 {
		t.Errorf("lookup ttl=%d remaining=%d want 300,300", res.TTL, res.TTLRemaining)
	}
	cl.advance(100 * time.Second)
	res = c.Lookup("example.com", TypeA)
	if res.TTLRemaining != 200 {
		t.Errorf("at 100s: remaining=%d want 200", res.TTLRemaining)
	}
}

func TestPutErrors(t *testing.T) {
	c, _ := testCache()
	cases := []struct {
		name   string
		t      Type
		ttl    int
		values []string
	}{
		{"negative ttl", TypeA, -1, []string{"1.1.1.1"}},
		{"bad ipv4", TypeA, 300, []string{"999.1.1.1"}},
		{"cname two values", TypeCNAME, 300, []string{"a.test", "b.test"}},
	}
	for _, tc := range cases {
		if err := c.Put("x.test", tc.t, tc.ttl, tc.values); err == nil {
			t.Errorf("%s: want error", tc.name)
		}
	}
}

func TestTTLExpiry(t *testing.T) {
	c, cl := testCache()
	_ = c.Put("a.test", TypeA, 300, []string{"1.1.1.1"})
	cl.advance(299 * time.Second)
	if c.Lookup("a.test", TypeA).Status != StatusNOERROR {
		t.Error("at 299s: want NOERROR")
	}
	cl.advance(1 * time.Second)
	if c.Lookup("a.test", TypeA).Status != StatusMISS {
		t.Error("at 300s: want MISS")
	}
}

func TestTTLZero(t *testing.T) {
	c, _ := testCache()
	_ = c.Put("z.test", TypeA, 0, []string{"1.1.1.1"})
	if c.Lookup("z.test", TypeA).Status != StatusMISS {
		t.Error("ttl=0: want MISS")
	}
}

func TestCNAMEChain(t *testing.T) {
	c, _ := testCache()
	_ = c.Put("alias.test", TypeCNAME, 600, []string{"target.test"})
	_ = c.Put("target.test", TypeA, 600, []string{"9.9.9.9"})
	res := c.Lookup("alias.test", TypeA)
	if res.Status != StatusNOERROR || len(res.Values) != 1 || res.Values[0] != "9.9.9.9" {
		t.Errorf("cname chain: %+v", res)
	}
	// Direct CNAME lookup returns the CNAME without following.
	res = c.Lookup("alias.test", TypeCNAME)
	if res.Status != StatusNOERROR || len(res.Values) != 1 || res.Values[0] != "target.test" {
		t.Errorf("direct cname lookup: %+v", res)
	}
}

func TestCNAMELoop(t *testing.T) {
	c, _ := testCache()
	_ = c.Put("a.test", TypeCNAME, 600, []string{"b.test"})
	_ = c.Put("b.test", TypeCNAME, 600, []string{"a.test"})
	if c.Lookup("a.test", TypeA).Status != StatusCNAMELoop {
		t.Error("a<->b: want CNAME_LOOP")
	}
	// Self-loop.
	_ = c.Put("self.test", TypeCNAME, 600, []string{"self.test"})
	if c.Lookup("self.test", TypeA).Status != StatusCNAMELoop {
		t.Error("self loop: want CNAME_LOOP")
	}
}

func TestCNAMEExactLimitTenHops(t *testing.T) {
	c, _ := testCache()
	for i := 1; i <= 10; i++ {
		_ = c.Put(fmt.Sprintf("c%d.test", i), TypeCNAME, 600, []string{fmt.Sprintf("c%d.test", i+1)})
	}
	_ = c.Put("c11.test", TypeA, 600, []string{"1.1.1.1"})
	if got := c.Lookup("c1.test", TypeA).Status; got != StatusNOERROR {
		t.Errorf("10 hops: got %v want NOERROR", got)
	}
}

func TestCNAMETooLongElevenHops(t *testing.T) {
	c, _ := testCache()
	for i := 1; i <= 11; i++ {
		_ = c.Put(fmt.Sprintf("c%d.test", i), TypeCNAME, 600, []string{fmt.Sprintf("c%d.test", i+1)})
	}
	_ = c.Put("c12.test", TypeA, 600, []string{"1.1.1.1"})
	if got := c.Lookup("c1.test", TypeA).Status; got != StatusCNAMETooLong {
		t.Errorf("11 hops: got %v want CNAME_TOO_LONG", got)
	}
}

func TestNXDOMAINCache(t *testing.T) {
	c, cl := testCache()
	_ = c.MarkNXDOMAIN("ghost.test", 60)
	if c.Lookup("ghost.test", TypeA).Status != StatusNXDOMAIN {
		t.Error("fresh nxdomain: want NXDOMAIN")
	}
	if c.Lookup("ghost.test", TypeTXT).Status != StatusNXDOMAIN {
		t.Error("nxdomain any type: want NXDOMAIN")
	}
	cl.advance(59 * time.Second)
	if c.Lookup("ghost.test", TypeA).Status != StatusNXDOMAIN {
		t.Error("at 59s: want NXDOMAIN")
	}
	cl.advance(1 * time.Second)
	if c.Lookup("ghost.test", TypeA).Status != StatusMISS {
		t.Error("at 60s: want MISS")
	}
}

func TestPutClearsNXDOMAIN(t *testing.T) {
	c, _ := testCache()
	_ = c.MarkNXDOMAIN("ghost.test", 60)
	_ = c.Put("ghost.test", TypeA, 300, []string{"1.1.1.1"})
	if c.Lookup("ghost.test", TypeA).Status != StatusNOERROR {
		t.Error("after put: want NOERROR")
	}
}

func TestEvictClearsNXDOMAIN(t *testing.T) {
	c, _ := testCache()
	_ = c.MarkNXDOMAIN("ghost.test", 60)
	c.Evict("ghost.test", TypeA) // evict by type also clears negative
	if c.Lookup("ghost.test", TypeA).Status != StatusMISS {
		t.Error("after evict by type: want MISS")
	}
}

func TestEvictByTypeAndByName(t *testing.T) {
	c, _ := testCache()
	_ = c.Put("m.test", TypeA, 300, []string{"1.1.1.1"})
	_ = c.Put("m.test", TypeAAAA, 300, []string{"::1"})
	if n, _ := c.Evict("m.test", TypeA); n != 1 {
		t.Errorf("evict A: removed %d want 1", n)
	}
	if c.Lookup("m.test", TypeA).Status != StatusMISS {
		t.Error("after evict A, lookup A: want MISS")
	}
	if c.Lookup("m.test", TypeAAAA).Status != StatusNOERROR {
		t.Error("after evict A, lookup AAAA: want NOERROR")
	}
	if n, _ := c.Evict("m.test", ""); n != 1 {
		t.Errorf("evict all: removed %d want 1 (AAAA left)", n)
	}
	if c.Lookup("m.test", TypeAAAA).Status != StatusMISS {
		t.Error("after evict all, lookup AAAA: want MISS")
	}
}

func TestReplaceOnPut(t *testing.T) {
	c, _ := testCache()
	_ = c.Put("r.test", TypeA, 300, []string{"1.1.1.1"})
	_ = c.Put("r.test", TypeA, 300, []string{"2.2.2.2", "3.3.3.3"})
	res := c.Lookup("r.test", TypeA)
	if len(res.Values) != 2 || res.Values[0] != "2.2.2.2" || res.Values[1] != "3.3.3.3" {
		t.Errorf("replace values=%v want [2.2.2.2 3.3.3.3]", res.Values)
	}
	st := c.Stats()
	if st.RecordSets != 1 {
		t.Errorf("replace stats record_sets=%d want 1", st.RecordSets)
	}
}

func TestStatsCounts(t *testing.T) {
	c, cl := testCache()
	_ = c.Put("a.test", TypeA, 300, []string{"1.1.1.1"})
	_ = c.Put("b.test", TypeA, 300, []string{"2.2.2.2"})
	_ = c.Put("c.test", TypeAAAA, 10, []string{"::1"})
	_ = c.MarkNXDOMAIN("ghost.test", 60)
	cl.advance(20 * time.Second)
	st := c.Stats()
	if st.RecordSets != 3 {
		t.Errorf("record_sets=%d want 3", st.RecordSets)
	}
	if st.ByType[TypeA] != 2 || st.ByType[TypeAAAA] != 1 || st.ByType[TypeCNAME] != 0 || st.ByType[TypeTXT] != 0 {
		t.Errorf("by_type=%v want A=2 AAAA=1 CNAME=0 TXT=0", st.ByType)
	}
	if st.Negative != 1 {
		t.Errorf("negative=%d want 1", st.Negative)
	}
	if st.Expired != 1 { // only c.test (AAAA) expired; negative still fresh
		t.Errorf("expired=%d want 1", st.Expired)
	}
}

func TestCaseInsensitiveNames(t *testing.T) {
	c, _ := testCache()
	_ = c.Put("Example.COM", TypeA, 300, []string{"1.1.1.1"})
	for _, q := range []string{"example.com", "EXAMPLE.COM", "ExAmPlE.CoM"} {
		if c.Lookup(q, TypeA).Status != StatusNOERROR {
			t.Errorf("lookup %q: want NOERROR", q)
		}
	}
}
