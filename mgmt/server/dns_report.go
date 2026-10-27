package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// adminDNS summarises DNS health from the "dns" / "dns_canceled" measurements (one per upstream
// query of the core, from xray's dnsLog) and the "dns_system" probes of the phone's own resolver.
func (s *Server) adminDNS(w http.ResponseWriter, r *http.Request) {
	win, err := s.parseWindow(r, 24*time.Hour)
	if err != nil {
		writeErr(w, err)
		return
	}
	where, args := win.where("ts")
	rows, err := s.st.db.Query(`SELECT kind, target, net, ok, ms, data, ts FROM measurements WHERE `+where+
		` AND kind IN ('dns', 'dns_canceled', 'dns_system')`, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	type group struct {
		Target   string  `json:"target"`
		Net      string  `json:"net"`
		N        int     `json:"n"`
		Ok       int     `json:"ok"`
		OkRate   float64 `json:"ok_rate"`
		P50      float64 `json:"p50_ms"`
		P90      float64 `json:"p90_ms"`
		LostRace int     `json:"lost_race,omitempty"`
		Blocked  int     `json:"blocked,omitempty"`
		Last     string  `json:"last"`
		ms       []float64
		lastTs   float64
	}
	type count struct {
		Key  string `json:"key"`
		N    int    `json:"n"`
		Last string `json:"last"`
		ts   float64
	}
	resolvers, system := map[string]*group{}, map[string]*group{}
	errs, domains := map[string]*count{}, map[string]*count{}
	bump := func(m map[string]*count, key string, ts float64) {
		c := m[key]
		if c == nil {
			c = &count{Key: key}
			m[key] = c
		}
		c.N++
		if ts > c.ts {
			c.ts = ts
		}
	}
	for rows.Next() {
		var kind, target, net, data string
		var ok int
		var ms, ts float64
		if err := rows.Scan(&kind, &target, &net, &ok, &ms, &data, &ts); err != nil {
			writeErr(w, err)
			return
		}
		var d struct {
			Domain  string `json:"domain"`
			Error   string `json:"error"`
			Blocked bool   `json:"blocked"`
		}
		json.Unmarshal([]byte(data), &d)
		groups := resolvers
		if kind == "dns_system" {
			groups = system
		}
		k := target + "\x00" + net
		g := groups[k]
		if g == nil {
			g = &group{Target: target, Net: net}
			groups[k] = g
		}
		if ts > g.lastTs {
			g.lastTs = ts
		}
		if kind == "dns_canceled" {
			g.LostRace++
			continue
		}
		g.N++
		if ok == 1 {
			g.Ok++
			if ms >= 0 {
				g.ms = append(g.ms, ms)
			}
			if d.Blocked {
				g.Blocked++
			}
			continue
		}
		e := d.Error
		if len(e) > 120 {
			e = e[:120] + "…"
		}
		bump(errs, target+" | "+e, ts)
		if d.Domain != "" {
			bump(domains, d.Domain, ts)
		} else if kind == "dns_system" {
			bump(domains, target+" (phone resolver)", ts)
		}
	}
	finish := func(m map[string]*group) []*group {
		out := make([]*group, 0, len(m))
		for _, g := range m {
			sort.Float64s(g.ms)
			if g.N > 0 {
				g.OkRate = float64(g.Ok) / float64(g.N)
			}
			g.P50, g.P90 = percentile(g.ms, 0.5), percentile(g.ms, 0.9)
			g.Last = s.iso(g.lastTs)
			out = append(out, g)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Net != out[j].Net {
				return out[i].Net < out[j].Net
			}
			return out[i].Target < out[j].Target
		})
		return out
	}
	top := func(m map[string]*count, n int) []*count {
		out := make([]*count, 0, len(m))
		for _, c := range m {
			c.Last = s.iso(c.ts)
			out = append(out, c)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].N > out[j].N || out[i].N == out[j].N && out[i].Key < out[j].Key })
		if len(out) > n {
			out = out[:n]
		}
		return out
	}
	res := map[string]any{
		"since":           s.iso(win.since),
		"resolvers":       finish(resolvers),
		"phone_resolver":  finish(system),
		"errors":          top(errs, 12),
		"failing_domains": top(domains, 12),
	}
	if r.URL.Query().Get("format") != "text" {
		writeJSON(w, 200, res)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "DNS since %s\n\nCore resolvers (one row per upstream query; lost = cancelled after the other resolver answered first)\n", res["since"])
	row := func(g *group) {
		ms := func(v float64) string {
			if v < 0 {
				return "      -  "
			}
			return fmt.Sprintf("%7.1fms", v)
		}
		fmt.Fprintf(&b, "  %-24s %-34s n=%-5d ok=%5.1f%%  p50=%s  p90=%s", g.Net, g.Target, g.N, 100*g.OkRate, ms(g.P50), ms(g.P90))
		if g.LostRace > 0 {
			fmt.Fprintf(&b, "  lost=%d", g.LostRace)
		}
		if g.Blocked > 0 {
			fmt.Fprintf(&b, "  blocked=%d", g.Blocked)
		}
		b.WriteString("\n")
	}
	for _, g := range finish(resolvers) {
		row(g)
	}
	b.WriteString("\nPhone resolver outside the VPN (dns_system probes, uncached; what bypassed apps use)\n")
	for _, g := range finish(system) {
		row(g)
	}
	b.WriteString("\nErrors\n")
	for _, c := range top(errs, 12) {
		fmt.Fprintf(&b, "  %4d  %s  (last %s)\n", c.N, c.Key, c.Last)
	}
	b.WriteString("\nFailing domains\n")
	for _, c := range top(domains, 12) {
		fmt.Fprintf(&b, "  %4d  %s  (last %s)\n", c.N, c.Key, c.Last)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(b.String()))
}
