// Package dnscache implements an in-memory, DNS-style record cache with
// TTL-based expiry, CNAME chain resolution, and NXDOMAIN negative caching.
//
// The cache is safe for concurrent use. The time source used for TTL
// evaluation is injectable at construction (NewWithClock) so that expiry
// behavior can be exercised deterministically without relying on real time;
// New uses the system clock.
package dnscache

import (
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// MaxCNAMEHops is the maximum number of CNAME redirections followed during a
// single lookup. The 11th hop yields StatusCNAMETooLong.
const MaxCNAMEHops = 10

// Type is a DNS record type supported by the cache.
type Type string

const (
	TypeA     Type = "A"
	TypeAAAA  Type = "AAAA"
	TypeCNAME Type = "CNAME"
	TypeTXT   Type = "TXT"
)

// validType reports whether t is a supported record type.
func validType(t Type) bool {
	switch t {
	case TypeA, TypeAAAA, TypeCNAME, TypeTXT:
		return true
	}
	return false
}

// ParseType normalizes a type string (case-insensitive) to a Type. The second
// return value is false for an unsupported type, in which case the returned
// Type is the empty zero value.
func ParseType(s string) (Type, bool) {
	t := Type(strings.ToUpper(s))
	if !validType(t) {
		return "", false
	}
	return t, true
}

// recordSet is a stored record collection: its type, original TTL, the instant
// it was inserted, its expiry instant, and its values.
type recordSet struct {
	typ        Type
	ttl        int
	insertedAt time.Time
	expiresAt  time.Time
	values     []string
}

// fresh reports whether the record set is still within its TTL at now.
func (rs *recordSet) fresh(now time.Time) bool {
	return now.Before(rs.expiresAt)
}

// remaining returns the whole seconds left before expiry, floored. It is only
// meaningful when fresh.
func (rs *recordSet) remaining(now time.Time) int {
	d := rs.expiresAt.Sub(now)
	if d < 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}

// negativeEntry is a cached NXDOMAIN response with its own TTL.
type negativeEntry struct {
	insertedAt time.Time
	expiresAt  time.Time
}

func (n *negativeEntry) fresh(now time.Time) bool {
	return now.Before(n.expiresAt)
}

// LookupStatus is the outcome of a Lookup.
type LookupStatus string

const (
	StatusNOERROR      LookupStatus = "NOERROR"
	StatusMISS         LookupStatus = "MISS"
	StatusNXDOMAIN     LookupStatus = "NXDOMAIN"
	StatusCNAMELoop    LookupStatus = "CNAME_LOOP"
	StatusCNAMETooLong LookupStatus = "CNAME_TOO_LONG"
)

// LookupResult is the result of a cache lookup.
type LookupResult struct {
	Status       LookupStatus
	Name         string
	Type         Type
	Values       []string
	TTL          int
	TTLRemaining int
}

// Cache is a concurrent-safe, in-memory DNS-style record cache.
type Cache struct {
	mu       sync.Mutex
	now      func() time.Time
	records  map[string]map[Type]*recordSet // name -> type -> recordSet
	negative map[string]*negativeEntry      // name -> NXDOMAIN
}

// New returns a Cache that uses the system clock for TTL evaluation.
func New() *Cache {
	return NewWithClock(time.Now)
}

// NewWithClock returns a Cache that evaluates TTL against the provided time
// source. The now function is consulted on every read and write. Passing nil
// falls back to the system clock.
func NewWithClock(now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{
		now:      now,
		records:  make(map[string]map[Type]*recordSet),
		negative: make(map[string]*negativeEntry),
	}
}

// NormalizeName lowercases and validates a domain name. It returns an error
// for empty, overlong, or character-set-invalid names. A trailing dot is not
// permitted.
func NormalizeName(s string) (string, error) {
	if len(s) == 0 || len(s) > 253 {
		return "", fmt.Errorf("invalid name %q: length must be 1..253", s)
	}
	if s[len(s)-1] == '.' {
		return "", fmt.Errorf("invalid name %q: trailing dot is not allowed", s)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
		default:
			return "", fmt.Errorf("invalid name %q: illegal character %q", s, r)
		}
	}
	return strings.ToLower(s), nil
}

// ValidateValues checks that values are well-formed for the given type and
// returns a normalized copy (CNAME targets are lowercased).
func ValidateValues(t Type, values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("values must not be empty")
	}
	out := make([]string, len(values))
	switch t {
	case TypeA:
		for i, v := range values {
			addr, err := netip.ParseAddr(v)
			if err != nil || !addr.Is4() {
				return nil, fmt.Errorf("invalid IPv4 %q", v)
			}
			out[i] = v
		}
	case TypeAAAA:
		for i, v := range values {
			addr, err := netip.ParseAddr(v)
			if err != nil || !addr.Is6() || addr.Is4In6() {
				return nil, fmt.Errorf("invalid IPv6 %q", v)
			}
			out[i] = v
		}
	case TypeCNAME:
		if len(values) != 1 {
			return nil, fmt.Errorf("CNAME requires exactly one value, got %d", len(values))
		}
		n, err := NormalizeName(values[0])
		if err != nil {
			return nil, fmt.Errorf("invalid CNAME target: %w", err)
		}
		out[0] = n
	case TypeTXT:
		for i, v := range values {
			if len(v) == 0 || len(v) > 255 {
				return nil, fmt.Errorf("TXT value length must be 1..255, got %d", len(v))
			}
			out[i] = v
		}
	default:
		return nil, fmt.Errorf("unsupported type %q", t)
	}
	return out, nil
}

// Put stores (or replaces) the record set for (name, type) and clears any
// NXDOMAIN negative cache for the name. ttl must be >= 0.
func (c *Cache) Put(name string, t Type, ttl int, values []string) error {
	n, err := NormalizeName(name)
	if err != nil {
		return err
	}
	if !validType(t) {
		return fmt.Errorf("unsupported type %q", t)
	}
	if ttl < 0 {
		return fmt.Errorf("ttl must be >= 0, got %d", ttl)
	}
	vals, err := ValidateValues(t, values)
	if err != nil {
		return err
	}
	now := c.now()
	rs := &recordSet{
		typ:        t,
		ttl:        ttl,
		insertedAt: now,
		expiresAt:  now.Add(time.Duration(ttl) * time.Second),
		values:     vals,
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.records[n] == nil {
		c.records[n] = make(map[Type]*recordSet)
	}
	c.records[n][t] = rs
	delete(c.negative, n) // a real record clears NXDOMAIN for this name
	return nil
}

// MarkNXDOMAIN caches a negative (name does not exist) response with its own
// TTL. ttl must be >= 0.
func (c *Cache) MarkNXDOMAIN(name string, ttl int) error {
	n, err := NormalizeName(name)
	if err != nil {
		return err
	}
	if ttl < 0 {
		return fmt.Errorf("ttl must be >= 0, got %d", ttl)
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.negative[n] = &negativeEntry{
		insertedAt: now,
		expiresAt:  now.Add(time.Duration(ttl) * time.Second),
	}
	return nil
}

// Lookup resolves name/type, following CNAME chains for non-CNAME queries.
// Expired records and negative entries encountered along the way are evicted
// lazily.
func (c *Cache) Lookup(name string, t Type) LookupResult {
	n, err := NormalizeName(name)
	if err != nil {
		return LookupResult{Status: StatusMISS, Name: name, Type: t}
	}
	if !validType(t) {
		return LookupResult{Status: StatusMISS, Name: n, Type: t}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()

	visited := make(map[string]bool)
	cur := n
	hops := 0
	for {
		if visited[cur] {
			return LookupResult{Status: StatusCNAMELoop, Name: n, Type: t}
		}
		visited[cur] = true

		// Negative cache takes precedence over everything else.
		if neg, ok := c.negative[cur]; ok {
			if neg.fresh(now) {
				return LookupResult{Status: StatusNXDOMAIN, Name: n, Type: t}
			}
			delete(c.negative, cur) // lazy evict expired negative entry
		}

		byType := c.records[cur]

		// Exact type match.
		if byType != nil {
			if rs, ok := byType[t]; ok {
				if rs.fresh(now) {
					values := append([]string(nil), rs.values...)
					if t == TypeTXT && len(values) == 1 {
						values = nil
					}
					if t == TypeAAAA {
						values = rs.values
					}
					return LookupResult{
						Status:       StatusNOERROR,
						Name:         n,
						Type:         t,
						Values:       values,
						TTL:          rs.ttl,
						TTLRemaining: rs.remaining(now),
					}
				}
				delete(byType, t) // lazy evict expired record
				if len(byType) == 0 {
					delete(c.records, cur)
					byType = nil
				}
			}
		}

		// Follow a CNAME for non-CNAME queries.
		if t != TypeCNAME && byType != nil {
			if cn, ok := byType[TypeCNAME]; ok {
				if cn.fresh(now) {
					hops++
					if hops > MaxCNAMEHops {
						return LookupResult{Status: StatusCNAMETooLong, Name: n, Type: t}
					}
					cur = cn.values[0]
					continue
				}
				delete(byType, TypeCNAME) // lazy evict expired CNAME
				if len(byType) == 0 {
					delete(c.records, cur)
				}
			}
		}

		return LookupResult{Status: StatusMISS, Name: n, Type: t}
	}
}

// Evict removes records for a name. If t is empty, all record sets for the
// name are removed; otherwise only the (name, t) record set is removed. In
// both cases the name's NXDOMAIN negative cache is cleared. It returns the
// number of record sets removed and whether a negative entry was cleared.
func (c *Cache) Evict(name string, t Type) (int, bool) {
	n, err := NormalizeName(name)
	if err != nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	evicted := 0
	if t == "" {
		if byType, ok := c.records[n]; ok {
			evicted = len(byType)
			delete(c.records, n)
		}
	} else if byType, ok := c.records[n]; ok {
		if _, ok := byType[t]; ok {
			delete(byType, t)
			evicted = 1
			if len(byType) == 0 {
				delete(c.records, n)
			}
		}
	}
	negCleared := false
	if _, ok := c.negative[n]; ok {
		delete(c.negative, n)
		negCleared = true
	}
	return evicted, negCleared
}

// Stats reports counts of stored record sets (by type), negative entries, and
// expired-but-not-yet-evicted entries. It does not mutate the cache.
type Stats struct {
	RecordSets int
	ByType     map[Type]int
	Negative   int
	Expired    int
}

// Stats returns a snapshot of cache contents. ByType always contains all four
// supported types (0 for absent types).
func (c *Cache) Stats() Stats {
	now := c.now()
	st := Stats{
		ByType: map[Type]int{TypeA: 0, TypeAAAA: 0, TypeCNAME: 0, TypeTXT: 0},
	}
	for _, byType := range c.records {
		for t, rs := range byType {
			st.RecordSets++
			st.ByType[t]++
			if !rs.fresh(now) {
				st.Expired++
			}
		}
	}
	for _, neg := range c.negative {
		st.Negative++
		if !neg.fresh(now) {
			st.Expired++
		}
	}
	return st
}
