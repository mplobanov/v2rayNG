package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Desired-configuration documents.
//
// A device's effective document is: profile chain (each profile may name a parent in
// "_extends") deep-merged, then the device's overrides, then temporary overrides that expire
// (log level). Objects merge key by key, everything else (arrays, scalars, null) replaces.
// The "telemetry" section tunes the device agent and is returned separately, so changing it
// never makes the device re-apply settings or restart the VPN. Keys starting with "_" are
// server-side annotations and are stripped before the document is sent.

func deepMerge(dst, src map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				dst[k] = deepMerge(cloneMap(dv), sv)
				continue
			}
			dst[k] = deepMerge(map[string]any{}, sv)
			continue
		}
		dst[k] = v
	}
	return dst
}

// mergePatch applies an RFC 7386 JSON merge patch: null deletes the key.
func mergePatch(dst map[string]any, patch map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		if pv, ok := v.(map[string]any); ok {
			dv, _ := dst[k].(map[string]any)
			dst[k] = mergePatch(cloneMap(dv), pv)
			continue
		}
		dst[k] = v
	}
	return dst
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	b, _ := json.Marshal(m)
	out := map[string]any{}
	json.Unmarshal(b, &out)
	return out
}

func parseObject(s string) (map[string]any, error) {
	if strings.TrimSpace(s) == "" {
		return map[string]any{}, nil
	}
	m := map[string]any{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return m, nil
}

func stripPrivate(m map[string]any) {
	for k, v := range m {
		if strings.HasPrefix(k, "_") {
			delete(m, k)
			continue
		}
		if sub, ok := v.(map[string]any); ok {
			stripPrivate(sub)
		}
	}
}

// canonicalHash hashes the JSON encoding; encoding/json sorts map keys, so equal documents
// always hash equally.
func canonicalHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func (s *Server) loadProfileChain(q queryer, name string) (map[string]any, []string, error) {
	var chain []string
	var docs []map[string]any
	seen := map[string]bool{}
	for name != "" {
		if seen[name] || len(chain) > 8 {
			return nil, chain, fmt.Errorf("profile cycle at %q", name)
		}
		seen[name] = true
		var raw string
		err := q.QueryRow(`SELECT doc FROM profiles WHERE name=?`, name).Scan(&raw)
		if err == sql.ErrNoRows {
			if len(chain) == 0 {
				// A device on a missing profile simply gets an empty document.
				return map[string]any{}, chain, nil
			}
			return nil, chain, fmt.Errorf("profile %q extends missing profile %q", chain[len(chain)-1], name)
		}
		if err != nil {
			return nil, chain, err
		}
		doc, err := parseObject(raw)
		if err != nil {
			return nil, chain, fmt.Errorf("profile %q: %w", name, err)
		}
		chain = append(chain, name)
		docs = append(docs, doc)
		parent, _ := doc["_extends"].(string)
		name = parent
	}
	out := map[string]any{}
	for i := len(docs) - 1; i >= 0; i-- {
		out = deepMerge(out, docs[i])
	}
	return out, chain, nil
}

type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

type Effective struct {
	Doc       map[string]any `json:"doc"`
	Telemetry map[string]any `json:"telemetry"`
	Version   string         `json:"version"`
	Profiles  []string       `json:"profiles"`
}

func (s *Server) effectiveConfig(q queryer, d *Device) (*Effective, error) {
	doc, chain, err := s.loadProfileChain(q, d.Profile)
	if err != nil {
		return nil, err
	}
	ov, err := parseObject(d.Overrides)
	if err != nil {
		return nil, fmt.Errorf("overrides: %w", err)
	}
	doc = deepMerge(doc, ov)
	now := time.Now().Unix()
	if d.LogLevel != "" && d.LogLevelUntil > now {
		doc = deepMerge(doc, map[string]any{"settings": map[string]any{"pref_core_loglevel": d.LogLevel}})
	}
	stripPrivate(doc)
	tel, _ := doc["telemetry"].(map[string]any)
	delete(doc, "telemetry")
	if tel == nil {
		tel = map[string]any{}
	}
	return &Effective{Doc: doc, Telemetry: tel, Version: canonicalHash(doc), Profiles: chain}, nil
}

// agentParams is what the device agent runs with until the next sync.
func (s *Server) agentParams(tel map[string]any, d *Device) map[string]any {
	a := map[string]any{
		"sync_sec":          300,
		"idle_sync_min":     15,
		"log_upload_sec":    60,
		"probe_sec":         900,
		"log_spec":          "*:W GoLog:V {app}:V",
		"log_buffer_lines":  20000,
		"probe_direct":      true,
		"probe_core_delay":  true,
		"dns_log":           true,
		"dns_log_cache":     false,
		"probe_dns":         "ya.ru youtube.com",
		"live_until":        0,
		"poll_wait_sec":     240,
		"upload_state_full": false,
	}
	if len(s.cfg.DeviceURLs) > 0 {
		a["mgmt_urls"] = s.cfg.DeviceURLs
	}
	for k, v := range tel {
		a[k] = v
	}
	if d.LiveUntil > time.Now().Unix() {
		a["live_until"] = d.LiveUntil
		a["log_upload_sec"] = 3
		a["sync_sec"] = 60
	}
	return a
}
