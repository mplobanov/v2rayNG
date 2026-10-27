package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ipCache resolves client IPs to ISP / AS names (ip-api.com, cached a week in SQLite), which
// is how measurements get grouped per access network.
type ipCache struct {
	st   *Store
	url  string
	mu   sync.Mutex
	mem  map[string]map[string]any
	busy map[string]bool
	http *http.Client
}

func newIPCache(st *Store, url string) *ipCache {
	return &ipCache{st: st, url: url, mem: map[string]map[string]any{}, busy: map[string]bool{},
		http: &http.Client{Timeout: 4 * time.Second}}
}

func (c *ipCache) get(ip string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mem[ip]
}

// shortName returns e.g. "MTS-AS" or "AS8359" for an IP, or "" when unknown.
func (c *ipCache) shortName(ip string) string {
	m := c.get(ip)
	if m == nil {
		return ""
	}
	if s, _ := m["asname"].(string); s != "" {
		return s
	}
	if s, _ := m["as"].(string); s != "" {
		return strings.Fields(s)[0]
	}
	if s, _ := m["isp"].(string); s != "" {
		return s
	}
	return ""
}

// lookup fills the cache for ip, blocking at most a few seconds on a cold miss.
func (c *ipCache) lookup(ip string) {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.IsLoopback() || parsed.IsPrivate() {
		return
	}
	c.mu.Lock()
	if _, ok := c.mem[ip]; ok {
		c.mu.Unlock()
		return
	}
	if c.busy[ip] {
		// Another request is resolving this IP; give it a moment so callers see the result.
		c.mu.Unlock()
		for i := 0; i < 20; i++ {
			time.Sleep(200 * time.Millisecond)
			if c.get(ip) != nil {
				return
			}
		}
		return
	}
	c.busy[ip] = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.busy, ip)
		c.mu.Unlock()
	}()

	var raw string
	var ts int64
	err := c.st.db.QueryRow(`SELECT data, ts FROM ipinfo WHERE ip=?`, ip).Scan(&raw, &ts)
	if err == nil && time.Now().Unix()-ts < 7*86400 {
		m := map[string]any{}
		if json.Unmarshal([]byte(raw), &m) == nil {
			c.mu.Lock()
			c.mem[ip] = m
			c.mu.Unlock()
			return
		}
	} else if err != nil && err != sql.ErrNoRows {
		log.Printf("ipinfo cache: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf(c.url, ip), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		log.Printf("ipinfo %s: %v", ip, err)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	m := map[string]any{}
	if json.Unmarshal(b, &m) != nil || m["status"] != "success" {
		log.Printf("ipinfo %s: %s", ip, strings.TrimSpace(string(b)))
		return
	}
	delete(m, "status")
	c.mu.Lock()
	c.mem[ip] = m
	if len(c.mem) > 5000 {
		c.mem = map[string]map[string]any{ip: m}
	}
	c.mu.Unlock()
	nb, _ := json.Marshal(m)
	c.st.Exec(`INSERT INTO ipinfo(ip, ts, data) VALUES(?,?,?) ON CONFLICT(ip) DO UPDATE SET ts=excluded.ts, data=excluded.data`,
		ip, time.Now().Unix(), string(nb))
}
