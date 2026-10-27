package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// resolveDevice accepts a full id, a unique id prefix, or a unique (case-insensitive) name.
func (s *Server) resolveDevice(ref string) (*Device, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errf(400, "device reference required")
	}
	rows, err := s.st.db.Query(`SELECT `+deviceCols+` FROM devices WHERE id=? OR id LIKE ? OR lower(name)=lower(?)`, ref, ref+"%", ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var exact *Device
	var all []*Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		if d.ID == ref || strings.EqualFold(d.Name, ref) {
			exact = d
		}
		all = append(all, d)
	}
	if exact != nil {
		return exact, nil
	}
	if len(all) == 1 {
		return all[0], nil
	}
	if len(all) == 0 {
		return nil, errf(404, "no device matches %q", ref)
	}
	return nil, errf(409, "%q is ambiguous (%d devices)", ref, len(all))
}

func (s *Server) withDevice(w http.ResponseWriter, r *http.Request) (*Device, bool) {
	d, err := s.resolveDevice(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return nil, false
	}
	return d, true
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func (s *Server) deviceSummary(d *Device) map[string]any {
	st, _ := parseObject(d.LastState)
	info, _ := parseObject(d.Info)
	now := time.Now().Unix()
	out := map[string]any{
		"id":              d.ID,
		"name":            d.Name,
		"status":          d.Status,
		"profile":         d.Profile,
		"enrolled_at":     s.iso(float64(d.EnrolledAt)),
		"last_seen":       s.iso(float64(d.LastSeen)),
		"last_seen_ago_s": now - d.LastSeen,
		"last_ip":         d.LastIP,
		"isp":             s.ipc.shortName(d.LastIP),
		"net":             st["net_label"],
		"vpn_running":     dig(st, "vpn", "running"),
		"server":          dig(st, "vpn", "selected", "remarks"),
		"model":           strings.TrimSpace(fmt.Sprint(dig(info, "manufacturer"), " ", dig(info, "model"))),
		"android":         dig(info, "android_release"),
		"app_version":     dig(info, "app_version"),
		"mgmt_url":        dig(info, "mgmt_url"),
		"applied_version": d.AppliedVersion,
		"note":            d.Note,
	}
	if d.LiveUntil > now {
		out["live_until"] = s.iso(float64(d.LiveUntil))
	}
	if d.LogLevel != "" && d.LogLevelUntil > now {
		out["loglevel_override"] = map[string]any{"level": d.LogLevel, "until": s.iso(float64(d.LogLevelUntil))}
	}
	if d.LastLogTs > 0 {
		out["last_log"] = s.iso(d.LastLogTs)
	}
	if eff, err := s.effectiveConfig(s.st.db, d); err == nil {
		out["config_version"] = eff.Version
		out["config_in_sync"] = eff.Version == d.AppliedVersion
	} else {
		out["config_error"] = err.Error()
	}
	return out
}

func (s *Server) allDevices() ([]*Device, error) {
	rows, err := s.st.db.Query(`SELECT ` + deviceCols + ` FROM devices ORDER BY last_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Server) adminDevices(w http.ResponseWriter, r *http.Request) {
	ds, err := s.allDevices()
	if err != nil {
		writeErr(w, err)
		return
	}
	out := []map[string]any{}
	for _, d := range ds {
		out = append(out, s.deviceSummary(d))
	}
	writeJSON(w, 200, out)
}

func (s *Server) adminDevice(w http.ResponseWriter, r *http.Request) {
	d, ok := s.withDevice(w, r)
	if !ok {
		return
	}
	out := s.deviceSummary(d)
	out["info"] = rawOrEmpty(d.Info)
	out["apply_result"] = rawOrEmpty(d.ApplyResult)
	out["overrides"] = rawOrEmpty(d.Overrides)
	writeJSON(w, 200, out)
}

func (s *Server) audit(tx *sql.Tx, target, action, before, after string) error {
	_, err := tx.Exec(`INSERT INTO audit(ts, target, action, before, after) VALUES(?,?,?,?,?)`, time.Now().Unix(), target, action, before, after)
	return err
}

func (s *Server) adminDevicePatch(w http.ResponseWriter, r *http.Request) {
	d, ok := s.withDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Name    *string `json:"name"`
		Profile *string `json:"profile"`
		Status  *string `json:"status"`
		Note    *string `json:"note"`
	}
	if err := readJSON(r, 64<<10, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Status != nil && *req.Status != "pending" && *req.Status != "approved" && *req.Status != "disabled" {
		writeErr(w, errf(400, "status must be pending, approved or disabled"))
		return
	}
	err := s.st.Write(func(tx *sql.Tx) error {
		before := fmt.Sprintf(`{"name":%q,"profile":%q,"status":%q,"note":%q}`, d.Name, d.Profile, d.Status, d.Note)
		if req.Name != nil {
			d.Name = strings.TrimSpace(*req.Name)
		}
		if req.Profile != nil {
			d.Profile = strings.TrimSpace(*req.Profile)
		}
		if req.Status != nil {
			d.Status = *req.Status
		}
		if req.Note != nil {
			d.Note = *req.Note
		}
		if _, err := tx.Exec(`UPDATE devices SET name=?, profile=?, status=?, note=? WHERE id=?`, d.Name, d.Profile, d.Status, d.Note, d.ID); err != nil {
			return err
		}
		after := fmt.Sprintf(`{"name":%q,"profile":%q,"status":%q,"note":%q}`, d.Name, d.Profile, d.Status, d.Note)
		return s.audit(tx, "device:"+d.ID, "patch", before, after)
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	s.notify.notify(d.ID)
	writeJSON(w, 200, s.deviceSummary(d))
}

func (s *Server) adminDeviceDelete(w http.ResponseWriter, r *http.Request) {
	d, ok := s.withDevice(w, r)
	if !ok {
		return
	}
	purge := r.URL.Query().Get("purge") == "1"
	err := s.st.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM devices WHERE id=?`, d.ID); err != nil {
			return err
		}
		if purge {
			for _, t := range []string{"states", "events", "measurements", "logs", "commands"} {
				if _, err := tx.Exec(`DELETE FROM `+t+` WHERE device_id=?`, d.ID); err != nil {
					return err
				}
			}
		}
		return s.audit(tx, "device:"+d.ID, "delete", d.Name, "")
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": d.ID, "purged": purge})
}

func (s *Server) adminDeviceConfig(w http.ResponseWriter, r *http.Request) {
	d, ok := s.withDevice(w, r)
	if !ok {
		return
	}
	eff, err := s.effectiveConfig(s.st.db, d)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"version": eff.Version, "applied_version": d.AppliedVersion, "in_sync": eff.Version == d.AppliedVersion,
		"profiles": eff.Profiles, "doc": eff.Doc, "telemetry": eff.Telemetry, "agent": s.agentParams(eff.Telemetry, d),
		"apply_result": rawOrEmpty(d.ApplyResult),
	})
}

func (s *Server) adminOverridesGet(w http.ResponseWriter, r *http.Request) {
	d, ok := s.withDevice(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(d.Overrides + "\n"))
}

func (s *Server) saveOverrides(w http.ResponseWriter, d *Device, doc map[string]any) {
	b, _ := json.Marshal(doc)
	err := s.st.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE devices SET overrides=? WHERE id=?`, string(b), d.ID); err != nil {
			return err
		}
		return s.audit(tx, "device:"+d.ID, "overrides", d.Overrides, string(b))
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	d.Overrides = string(b)
	s.notify.notify(d.ID)
	eff, err := s.effectiveConfig(s.st.db, d)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"overrides": doc, "config_version": eff.Version})
}

func (s *Server) adminOverridesPut(w http.ResponseWriter, r *http.Request) {
	d, ok := s.withDevice(w, r)
	if !ok {
		return
	}
	doc := map[string]any{}
	if err := readJSON(r, 4<<20, &doc); err != nil {
		writeErr(w, err)
		return
	}
	s.saveOverrides(w, d, doc)
}

func (s *Server) adminOverridesPatch(w http.ResponseWriter, r *http.Request) {
	d, ok := s.withDevice(w, r)
	if !ok {
		return
	}
	patch := map[string]any{}
	if err := readJSON(r, 4<<20, &patch); err != nil {
		writeErr(w, err)
		return
	}
	cur, err := parseObject(d.Overrides)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.saveOverrides(w, d, mergePatch(cur, patch))
}

func (s *Server) adminDeviceState(w http.ResponseWriter, r *http.Request) {
	d, ok := s.withDevice(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(d.LastState + "\n"))
}

func (s *Server) adminDeviceStates(w http.ResponseWriter, r *http.Request) {
	d, ok := s.withDevice(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	since, err := s.parseWhen(q.Get("since"), float64(time.Now().Unix()-6*3600))
	if err != nil {
		writeErr(w, err)
		return
	}
	until, err := s.parseWhen(q.Get("until"), float64(time.Now().Unix()+60))
	if err != nil {
		writeErr(w, err)
		return
	}
	limit := intParam(r, "limit", 200, 5000)
	rows, err := s.st.db.Query(`SELECT ts, ip, net, vpn, state FROM states WHERE device_id=? AND ts>=? AND ts<=? ORDER BY ts DESC LIMIT ?`, d.ID, since, until, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var ts int64
		var ip, net, state string
		var vpn int
		if err := rows.Scan(&ts, &ip, &net, &vpn, &state); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, map[string]any{"ts": ts, "time": s.iso(float64(ts)), "ip": ip, "net": net, "vpn": vpn == 1, "state": rawOrEmpty(state)})
	}
	writeJSON(w, 200, out)
}

// ---------- commands ----------

var knownCommands = map[string]bool{
	"sync": true, "upload_logs": true, "restart_vpn": true, "stop_vpn": true, "start_vpn": true,
	"probe": true, "update_subscriptions": true, "update_geo": true, "select_server": true,
	"exec": true, "dump_state": true, "reenroll": true,
}

func (s *Server) adminCommandCreate(w http.ResponseWriter, r *http.Request) {
	d, ok := s.withDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Type   string          `json:"type"`
		Args   json.RawMessage `json:"args"`
		TTLSec int64           `json:"ttl_sec"`
	}
	if err := readJSON(r, 1<<20, &req); err != nil {
		writeErr(w, err)
		return
	}
	if !knownCommands[req.Type] {
		writeErr(w, errf(400, "unknown command %q", req.Type))
		return
	}
	if req.TTLSec <= 0 {
		req.TTLSec = 3600
	}
	args := string(req.Args)
	if args == "" || args == "null" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		writeErr(w, errf(400, "args must be JSON"))
		return
	}
	now := time.Now().Unix()
	var id int64
	err := s.st.Write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO commands(device_id, created, expires, type, args) VALUES(?,?,?,?,?)`, d.ID, now, now+req.TTLSec, req.Type, args)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	s.notify.notify(d.ID)
	if wait := intParam(r, "wait", 0, 600); wait > 0 {
		s.writeCommand(w, id, wait, r)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "device": d.ID, "type": req.Type, "status": "queued",
		"note": "delivered at the device's next sync or immediately if it is long-polling"})
}

func (s *Server) loadCommand(id int64) (map[string]any, string, error) {
	var devID, typ, args, status, result, errS string
	var created, expires, sent, deliveries, done int64
	err := s.st.db.QueryRow(`SELECT device_id, created, expires, type, args, status, sent_at, deliveries, done_at, result, error FROM commands WHERE id=?`, id).
		Scan(&devID, &created, &expires, &typ, &args, &status, &sent, &deliveries, &done, &result, &errS)
	if err == sql.ErrNoRows {
		return nil, "", errf(404, "no command %d", id)
	}
	if err != nil {
		return nil, "", err
	}
	out := map[string]any{"id": id, "device": devID, "type": typ, "args": rawOrEmpty(args), "status": status,
		"created": s.iso(float64(created)), "expires": s.iso(float64(expires))}
	if sent > 0 {
		out["sent_at"] = s.iso(float64(sent))
		out["deliveries"] = deliveries
	}
	if done > 0 {
		out["done_at"] = s.iso(float64(done))
		out["result"] = rawOrEmpty(result)
		if errS != "" {
			out["error"] = errS
		}
	}
	return out, status, nil
}

func (s *Server) writeCommand(w http.ResponseWriter, id int64, wait int, r *http.Request) {
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		c, status, err := s.loadCommand(id)
		if err != nil {
			writeErr(w, err)
			return
		}
		if status == "done" || status == "failed" || status == "expired" || time.Now().After(deadline) {
			writeJSON(w, 200, c)
			return
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) adminCommand(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil {
		writeErr(w, errf(400, "bad command id"))
		return
	}
	s.writeCommand(w, id, intParam(r, "wait", 0, 600), r)
}

func (s *Server) adminCommands(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	where, args := "1=1", []any{}
	if ref := q.Get("device"); ref != "" {
		d, err := s.resolveDevice(ref)
		if err != nil {
			writeErr(w, err)
			return
		}
		where += " AND device_id=?"
		args = append(args, d.ID)
	}
	args = append(args, intParam(r, "limit", 50, 1000))
	rows, err := s.st.db.Query(`SELECT id FROM commands WHERE `+where+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	out := []map[string]any{}
	for _, id := range ids {
		if c, _, err := s.loadCommand(id); err == nil {
			out = append(out, c)
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) adminLive(w http.ResponseWriter, r *http.Request) {
	d, ok := s.withDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Minutes float64 `json:"minutes"`
	}
	if err := readJSON(r, 4<<10, &req); err != nil {
		writeErr(w, err)
		return
	}
	until := int64(0)
	if req.Minutes > 0 {
		until = time.Now().Unix() + int64(req.Minutes*60)
	}
	if _, err := s.st.Exec(`UPDATE devices SET live_until=? WHERE id=?`, until, d.ID); err != nil {
		writeErr(w, err)
		return
	}
	s.notify.notify(d.ID)
	writeJSON(w, 200, map[string]any{"device": d.ID, "live_until": until,
		"note": "takes effect at the device's next sync/poll (immediately if it is long-polling)"})
}

var validLogLevels = map[string]bool{"debug": true, "info": true, "warning": true, "error": true, "none": true}

func (s *Server) adminLogLevel(w http.ResponseWriter, r *http.Request) {
	d, ok := s.withDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Level   string  `json:"level"`
		Minutes float64 `json:"minutes"`
	}
	if err := readJSON(r, 4<<10, &req); err != nil {
		writeErr(w, err)
		return
	}
	until := int64(0)
	if req.Level != "" {
		if !validLogLevels[req.Level] {
			writeErr(w, errf(400, "level must be one of debug, info, warning, error, none"))
			return
		}
		if req.Minutes <= 0 {
			req.Minutes = 60
		}
		until = time.Now().Unix() + int64(req.Minutes*60)
	}
	if _, err := s.st.Exec(`UPDATE devices SET loglevel=?, loglevel_until=? WHERE id=?`, req.Level, until, d.ID); err != nil {
		writeErr(w, err)
		return
	}
	s.notify.notify(d.ID)
	writeJSON(w, 200, map[string]any{"device": d.ID, "level": req.Level, "until": until,
		"note": "the core restarts on the device to apply it; the override expires by itself"})
}

// ---------- profiles ----------

func (s *Server) adminProfiles(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.db.Query(`SELECT name, doc, updated_at FROM profiles ORDER BY name`)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name, doc string
		var ts int64
		rows.Scan(&name, &doc, &ts)
		m, _ := parseObject(doc)
		out = append(out, map[string]any{"name": name, "extends": m["_extends"], "updated_at": s.iso(float64(ts)), "bytes": len(doc)})
	}
	writeJSON(w, 200, out)
}

func (s *Server) adminProfileGet(w http.ResponseWriter, r *http.Request) {
	var doc string
	err := s.st.db.QueryRow(`SELECT doc FROM profiles WHERE name=?`, r.PathValue("name")).Scan(&doc)
	if err == sql.ErrNoRows {
		writeErr(w, errf(404, "no profile %q", r.PathValue("name")))
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(doc + "\n"))
}

func (s *Server) saveProfile(w http.ResponseWriter, name string, doc map[string]any) {
	b, _ := json.MarshalIndent(doc, "", "  ")
	err := s.st.Write(func(tx *sql.Tx) error {
		var before string
		tx.QueryRow(`SELECT doc FROM profiles WHERE name=?`, name).Scan(&before)
		if _, err := tx.Exec(`INSERT INTO profiles(name, doc, updated_at) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET doc=excluded.doc, updated_at=excluded.updated_at`,
			name, string(b), time.Now().Unix()); err != nil {
			return err
		}
		// Validate the chain now so a broken _extends is reported to the editor, not the devices.
		if _, _, err := s.loadProfileChain(tx, name); err != nil {
			return errf(400, "%v", err)
		}
		return s.audit(tx, "profile:"+name, "save", before, string(b))
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	s.notify.notifyAll()
	writeJSON(w, 200, map[string]any{"name": name, "saved": true, "bytes": len(b)})
}

func (s *Server) adminProfilePut(w http.ResponseWriter, r *http.Request) {
	doc := map[string]any{}
	if err := readJSON(r, 4<<20, &doc); err != nil {
		writeErr(w, err)
		return
	}
	s.saveProfile(w, r.PathValue("name"), doc)
}

func (s *Server) adminProfilePatch(w http.ResponseWriter, r *http.Request) {
	patch := map[string]any{}
	if err := readJSON(r, 4<<20, &patch); err != nil {
		writeErr(w, err)
		return
	}
	var raw string
	s.st.db.QueryRow(`SELECT doc FROM profiles WHERE name=?`, r.PathValue("name")).Scan(&raw)
	cur, err := parseObject(raw)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.saveProfile(w, r.PathValue("name"), mergePatch(cur, patch))
}

func (s *Server) adminProfileDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.st.Write(func(tx *sql.Tx) error {
		var before string
		tx.QueryRow(`SELECT doc FROM profiles WHERE name=?`, name).Scan(&before)
		if _, err := tx.Exec(`DELETE FROM profiles WHERE name=?`, name); err != nil {
			return err
		}
		return s.audit(tx, "profile:"+name, "delete", before, "")
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	s.notify.notifyAll()
	writeJSON(w, 200, map[string]any{"deleted": name})
}

func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.db.Query(`SELECT ts, target, action, before, after FROM audit ORDER BY id DESC LIMIT ?`, intParam(r, "limit", 50, 1000))
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var ts int64
		var target, action, before, after string
		rows.Scan(&ts, &target, &action, &before, &after)
		out = append(out, map[string]any{"time": s.iso(float64(ts)), "target": target, "action": action, "before": rawOrEmpty(before), "after": rawOrEmpty(after)})
	}
	writeJSON(w, 200, out)
}

func (s *Server) adminIPInfo(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	s.ipc.lookup(ip)
	writeJSON(w, 200, s.ipc.get(ip))
}

// ---------- logs / events / measurements / stats / timeline ----------

type window struct {
	devID        string
	since, until float64
}

func (s *Server) parseWindow(r *http.Request, defSince time.Duration) (*window, error) {
	q := r.URL.Query()
	win := &window{}
	if ref := q.Get("device"); ref != "" {
		d, err := s.resolveDevice(ref)
		if err != nil {
			return nil, err
		}
		win.devID = d.ID
	}
	now := float64(time.Now().UnixNano()) / 1e9
	var err error
	if win.since, err = s.parseWhen(q.Get("since"), now-defSince.Seconds()); err != nil {
		return nil, err
	}
	if win.until, err = s.parseWhen(q.Get("until"), now+60); err != nil {
		return nil, err
	}
	return win, nil
}

func (win *window) where(col string) (string, []any) {
	w := col + ">=? AND " + col + "<=?"
	args := []any{win.since, win.until}
	if win.devID != "" {
		w += " AND device_id=?"
		args = append(args, win.devID)
	}
	return w, args
}

var levelOrder = []string{"V", "D", "I", "W", "E", "F"}

func levelsAtLeast(min string) []string {
	min = strings.ToUpper(strings.TrimSpace(min))
	if min == "" {
		return nil
	}
	for i, l := range levelOrder {
		if strings.HasPrefix(min, l) {
			return levelOrder[i:]
		}
	}
	return nil
}

func (s *Server) deviceNames() map[string]string {
	out := map[string]string{}
	rows, err := s.st.db.Query(`SELECT id, name FROM devices`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		rows.Scan(&id, &name)
		if name == "" {
			name = id[:8]
		}
		out[id] = name
	}
	return out
}

func (s *Server) adminLogs(w http.ResponseWriter, r *http.Request) {
	win, err := s.parseWindow(r, time.Hour)
	if err != nil {
		writeErr(w, err)
		return
	}
	q := r.URL.Query()
	where, args := win.where("ts")
	if lv := levelsAtLeast(q.Get("level")); lv != nil {
		where += " AND lvl IN (" + strings.TrimSuffix(strings.Repeat("?,", len(lv)), ",") + ")"
		for _, l := range lv {
			args = append(args, l)
		}
	}
	if tags := q.Get("tag"); tags != "" {
		parts := strings.Split(tags, ",")
		where += " AND tag IN (" + strings.TrimSuffix(strings.Repeat("?,", len(parts)), ",") + ")"
		for _, t := range parts {
			args = append(args, strings.TrimSpace(t))
		}
	}
	for _, term := range q["q"] {
		where += " AND msg LIKE ?"
		args = append(args, "%"+term+"%")
	}
	if ex := q.Get("exclude"); ex != "" {
		where += " AND msg NOT LIKE ?"
		args = append(args, "%"+ex+"%")
	}
	limit := intParam(r, "limit", 500, 50000)
	order := "DESC"
	if q.Get("order") == "asc" {
		order = "ASC"
	}
	args = append(args, limit)
	rows, err := s.st.db.Query(`SELECT device_id, ts, pid, lvl, tag, msg FROM logs WHERE `+where+` ORDER BY ts `+order+`, id `+order+` LIMIT ?`, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	names := s.deviceNames()
	type row struct {
		Dev  string  `json:"device"`
		Ts   float64 `json:"ts"`
		Time string  `json:"time"`
		Pid  int     `json:"pid"`
		Lvl  string  `json:"lvl"`
		Tag  string  `json:"tag"`
		Msg  string  `json:"msg"`
	}
	out := []row{}
	for rows.Next() {
		var x row
		var dev string
		if err := rows.Scan(&dev, &x.Ts, &x.Pid, &x.Lvl, &x.Tag, &x.Msg); err != nil {
			writeErr(w, err)
			return
		}
		x.Dev = names[dev]
		x.Time = s.iso(x.Ts)
		out = append(out, x)
	}
	if order == "DESC" {
		// newest N, printed oldest first
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	if q.Get("format") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		var sb strings.Builder
		for _, x := range out {
			fmt.Fprintf(&sb, "%s %-10s %s %s: %s\n", x.Time[5:23], x.Dev, x.Lvl, x.Tag, x.Msg)
		}
		w.Write([]byte(sb.String()))
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) adminEvents(w http.ResponseWriter, r *http.Request) {
	win, err := s.parseWindow(r, 24*time.Hour)
	if err != nil {
		writeErr(w, err)
		return
	}
	where, args := win.where("ts")
	if t := r.URL.Query().Get("type"); t != "" {
		where += " AND type=?"
		args = append(args, t)
	}
	args = append(args, intParam(r, "limit", 500, 20000))
	rows, err := s.st.db.Query(`SELECT device_id, ts, type, data FROM events WHERE `+where+` ORDER BY ts DESC LIMIT ?`, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	names := s.deviceNames()
	out := []map[string]any{}
	for rows.Next() {
		var dev, typ, data string
		var ts float64
		rows.Scan(&dev, &ts, &typ, &data)
		out = append(out, map[string]any{"device": names[dev], "ts": ts, "time": s.iso(ts), "type": typ, "data": rawOrEmpty(data)})
	}
	writeJSON(w, 200, out)
}

func (s *Server) measWhere(r *http.Request, win *window) (string, []any) {
	where, args := win.where("ts")
	q := r.URL.Query()
	if k := q.Get("kind"); k != "" {
		parts := strings.Split(k, ",")
		where += " AND kind IN (" + strings.TrimSuffix(strings.Repeat("?,", len(parts)), ",") + ")"
		for _, p := range parts {
			args = append(args, strings.TrimSpace(p))
		}
	}
	if t := q.Get("target"); t != "" {
		where += " AND target LIKE ?"
		args = append(args, "%"+t+"%")
	}
	if n := q.Get("net"); n != "" {
		where += " AND net LIKE ?"
		args = append(args, "%"+n+"%")
	}
	return where, args
}

func (s *Server) adminMeasurements(w http.ResponseWriter, r *http.Request) {
	win, err := s.parseWindow(r, 24*time.Hour)
	if err != nil {
		writeErr(w, err)
		return
	}
	where, args := s.measWhere(r, win)
	args = append(args, intParam(r, "limit", 500, 50000))
	rows, err := s.st.db.Query(`SELECT device_id, ts, kind, target, ok, ms, net, ip, data FROM measurements WHERE `+where+` ORDER BY ts DESC LIMIT ?`, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	names := s.deviceNames()
	out := []map[string]any{}
	for rows.Next() {
		var dev, kind, target, net, ip, data string
		var ts, ms float64
		var ok int
		rows.Scan(&dev, &ts, &kind, &target, &ok, &ms, &net, &ip, &data)
		out = append(out, map[string]any{"device": names[dev], "time": s.iso(ts), "kind": kind, "target": target, "ok": ok == 1,
			"ms": ms, "net": net, "ip": ip, "data": rawOrEmpty(data)})
	}
	writeJSON(w, 200, out)
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return -1
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}

// adminStats aggregates measurements: success rate and latency percentiles per group.
func (s *Server) adminStats(w http.ResponseWriter, r *http.Request) {
	win, err := s.parseWindow(r, 24*time.Hour)
	if err != nil {
		writeErr(w, err)
		return
	}
	by := strings.Split(r.URL.Query().Get("by"), ",")
	if r.URL.Query().Get("by") == "" {
		by = []string{"kind", "target", "net"}
	}
	where, args := s.measWhere(r, win)
	rows, err := s.st.db.Query(`SELECT device_id, kind, target, net, ok, ms, ts FROM measurements WHERE `+where, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	names := s.deviceNames()
	type agg struct {
		key        map[string]string
		n, ok      int
		ms         []float64
		first, last float64
	}
	groups := map[string]*agg{}
	for rows.Next() {
		var dev, kind, target, net string
		var ok int
		var ms, ts float64
		rows.Scan(&dev, &kind, &target, &net, &ok, &ms, &ts)
		vals := map[string]string{"device": names[dev], "kind": kind, "target": target, "net": net,
			"hour": time.Unix(int64(ts), 0).In(s.loc).Format("2006-01-02 15h"),
			"day":  time.Unix(int64(ts), 0).In(s.loc).Format("2006-01-02")}
		key := map[string]string{}
		var kparts []string
		for _, b := range by {
			b = strings.TrimSpace(b)
			key[b] = vals[b]
			kparts = append(kparts, vals[b])
		}
		k := strings.Join(kparts, "\x00")
		g := groups[k]
		if g == nil {
			g = &agg{key: key, first: ts}
			groups[k] = g
		}
		g.n++
		if ok == 1 {
			g.ok++
			if ms >= 0 {
				g.ms = append(g.ms, ms)
			}
		}
		if ts < g.first {
			g.first = ts
		}
		if ts > g.last {
			g.last = ts
		}
	}
	out := []map[string]any{}
	for _, g := range groups {
		sort.Float64s(g.ms)
		m := map[string]any{"n": g.n, "ok": g.ok, "ok_rate": float64(g.ok) / float64(g.n),
			"p50_ms": percentile(g.ms, 0.5), "p90_ms": percentile(g.ms, 0.9), "max_ms": percentile(g.ms, 1),
			"first": s.iso(g.first), "last": s.iso(g.last)}
		for k, v := range g.key {
			m[k] = v
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		for _, b := range by {
			a, _ := out[i][strings.TrimSpace(b)].(string)
			c, _ := out[j][strings.TrimSpace(b)].(string)
			if a != c {
				return a < c
			}
		}
		return false
	})
	writeJSON(w, 200, out)
}

type tlEntry struct {
	Ts      float64 `json:"ts"`
	Time    string  `json:"time"`
	Device  string  `json:"device"`
	Kind    string  `json:"kind"`
	Summary string  `json:"summary"`
	Data    any     `json:"data,omitempty"`
}

func compactJSON(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// adminTimeline merges what happened on a device into one chronological list: state changes
// (network/ISP/VPN on-off), events, measurements, commands and warning+ log lines.
func (s *Server) adminTimeline(w http.ResponseWriter, r *http.Request) {
	win, err := s.parseWindow(r, 6*time.Hour)
	if err != nil {
		writeErr(w, err)
		return
	}
	q := r.URL.Query()
	names := s.deviceNames()
	var out []tlEntry
	add := func(ts float64, dev, kind, summary string, data any) {
		out = append(out, tlEntry{Ts: ts, Time: s.iso(ts), Device: names[dev], Kind: kind, Summary: summary, Data: data})
	}
	where, args := win.where("ts")

	// state transitions
	rows, err := s.st.db.Query(`SELECT device_id, ts, ip, net, vpn, state FROM states WHERE `+where+` ORDER BY ts`, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	prev := map[string]string{}
	for rows.Next() {
		var dev, ip, net, state string
		var ts int64
		var vpn int
		rows.Scan(&dev, &ts, &ip, &net, &vpn, &state)
		st, _ := parseObject(state)
		server := fmt.Sprint(dig(st, "vpn", "selected", "remarks"))
		sig := fmt.Sprintf("ip=%s net=%s vpn=%v server=%s", ip, net, vpn == 1, server)
		if prev[dev] != sig {
			add(float64(ts), dev, "state", sig, nil)
			prev[dev] = sig
		}
	}
	rows.Close()

	rows, err = s.st.db.Query(`SELECT device_id, ts, type, data FROM events WHERE `+where+` ORDER BY ts`, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	for rows.Next() {
		var dev, typ, data string
		var ts float64
		rows.Scan(&dev, &ts, &typ, &data)
		add(ts, dev, "event", typ+" "+compactJSON(data, 400), nil)
	}
	rows.Close()

	// Per-query DNS rows would bury everything else; the timeline keeps only failed lookups.
	rows, err = s.st.db.Query(`SELECT device_id, ts, kind, target, ok, ms, data FROM measurements WHERE `+where+
		` AND kind != 'dns_canceled' AND NOT (kind = 'dns' AND ok = 1) ORDER BY ts`, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	for rows.Next() {
		var dev, kind, target, data string
		var ts, ms float64
		var ok int
		rows.Scan(&dev, &ts, &kind, &target, &ok, &ms, &data)
		res := "FAIL"
		if ok == 1 {
			res = fmt.Sprintf("ok %.0fms", ms)
		}
		extra := ""
		if ok == 0 {
			if m, _ := parseObject(data); m != nil && m["error"] != nil {
				extra = " " + compactJSON(fmt.Sprint(m["error"]), 160)
			}
		}
		add(ts, dev, "measure", fmt.Sprintf("%s %s %s%s", kind, target, res, extra), nil)
	}
	rows.Close()

	cw, cargs := win.where("created")
	rows, err = s.st.db.Query(`SELECT device_id, created, id, type, status, error FROM commands WHERE `+cw+` ORDER BY created`, cargs...)
	if err != nil {
		writeErr(w, err)
		return
	}
	for rows.Next() {
		var dev, typ, status, errS string
		var created, id int64
		rows.Scan(&dev, &created, &id, &typ, &status, &errS)
		add(float64(created), dev, "command", fmt.Sprintf("#%d %s -> %s %s", id, typ, status, errS), nil)
	}
	rows.Close()

	minLvl := q.Get("log_level")
	if minLvl == "" {
		minLvl = "W"
	}
	if minLvl != "none" {
		lv := levelsAtLeast(minLvl)
		lw := where + " AND lvl IN (" + strings.TrimSuffix(strings.Repeat("?,", len(lv)), ",") + ")"
		largs := append([]any{}, args...)
		for _, l := range lv {
			largs = append(largs, l)
		}
		largs = append(largs, intParam(r, "log_limit", 2000, 50000))
		rows, err = s.st.db.Query(`SELECT device_id, ts, lvl, tag, msg FROM logs WHERE `+lw+` ORDER BY ts DESC, id DESC LIMIT ?`, largs...)
		if err != nil {
			writeErr(w, err)
			return
		}
		var logs []tlEntry
		for rows.Next() {
			var dev, lvl, tag, msg string
			var ts float64
			rows.Scan(&dev, &ts, &lvl, &tag, &msg)
			logs = append(logs, tlEntry{Ts: ts, Time: s.iso(ts), Device: names[dev], Kind: "log", Summary: lvl + " " + tag + ": " + compactJSON(msg, 600)})
		}
		rows.Close()
		// newest N were fetched; keep each burst (e.g. a stack trace) in its original order
		for i := len(logs) - 1; i >= 0; i-- {
			out = append(out, logs[i])
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Ts < out[j].Ts })
	if q.Get("format") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		var sb strings.Builder
		for _, e := range out {
			fmt.Fprintf(&sb, "%s %-10s %-7s %s\n", e.Time[5:19], e.Device, e.Kind, e.Summary)
		}
		w.Write([]byte(sb.String()))
		return
	}
	if out == nil {
		out = []tlEntry{}
	}
	writeJSON(w, 200, out)
}
