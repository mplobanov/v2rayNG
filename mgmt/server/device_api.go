package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var deviceIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

type enrollReq struct {
	EnrollToken string          `json:"enroll_token"`
	DeviceID    string          `json:"device_id"`
	Name        string          `json:"name"`
	Device      json.RawMessage `json:"device"`
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.enrollRL.allow(ip) {
		writeJSON(w, 429, map[string]any{"error": "too many enrollments"})
		return
	}
	var req enrollReq
	if err := readJSON(r, 256<<10, &req); err != nil {
		writeErr(w, err)
		return
	}
	if !tokenMatches(req.EnrollToken, s.cfg.EnrollTokenSHA256) {
		time.Sleep(500 * time.Millisecond)
		writeJSON(w, 403, map[string]any{"error": "bad enrollment token"})
		return
	}
	if !deviceIDRe.MatchString(req.DeviceID) {
		writeJSON(w, 400, map[string]any{"error": "bad device_id"})
		return
	}
	tok := randomToken()
	now := time.Now().Unix()
	status := "pending"
	if s.cfg.AutoApproveUntil > now {
		status = "approved"
	}
	info := string(req.Device)
	if !json.Valid([]byte(info)) {
		info = "{}"
	}
	name := strings.TrimSpace(req.Name)
	err := s.st.Write(func(tx *sql.Tx) error {
		var existing string
		err := tx.QueryRow(`SELECT status FROM devices WHERE id=?`, req.DeviceID).Scan(&existing)
		if err == sql.ErrNoRows {
			_, err = tx.Exec(`INSERT INTO devices(id, token_hash, name, status, profile, enrolled_at, last_seen, last_ip, info)
				VALUES(?,?,?,?,?,?,?,?,?)`, req.DeviceID, sha256Hex(tok), name, status, s.cfg.DefaultProfile, now, now, ip, info)
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			status = existing
			if status == "disabled" {
				return errf(403, "device disabled")
			}
			if _, err := tx.Exec(`UPDATE devices SET token_hash=?, last_seen=?, last_ip=?, info=? WHERE id=?`,
				sha256Hex(tok), now, ip, info, req.DeviceID); err != nil {
				return err
			}
		}
		data, _ := json.Marshal(map[string]any{"ip": ip, "name": name, "status": status})
		_, err = tx.Exec(`INSERT INTO events(device_id, ts, type, data) VALUES(?,?,?,?)`, req.DeviceID, float64(now), "enrolled", string(data))
		return err
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	go s.ipc.lookup(ip)
	writeJSON(w, 200, map[string]any{"device_token": tok, "status": status, "device_id": req.DeviceID})
}

type eventIn struct {
	Ts   float64         `json:"ts"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type measIn struct {
	Ts     float64         `json:"ts"`
	Kind   string          `json:"kind"`
	Target string          `json:"target"`
	Ok     bool            `json:"ok"`
	Ms     float64         `json:"ms"`
	Data   json.RawMessage `json:"data"`
}

type resultIn struct {
	ID           int64           `json:"id"`
	Ok           bool            `json:"ok"`
	Result       json.RawMessage `json:"result"`
	Error        string          `json:"error"`
	Measurements []measIn        `json:"measurements"`
}

type syncReq struct {
	Name           string          `json:"name"`
	Device         json.RawMessage `json:"device"`
	AppliedVersion string          `json:"applied_version"`
	ApplyResult    json.RawMessage `json:"apply_result"`
	State          map[string]any  `json:"state"`
	Events         []eventIn       `json:"events"`
	Measurements   []measIn        `json:"measurements"`
	Results        []resultIn      `json:"results"`
}

type cmdOut struct {
	ID      int64           `json:"id"`
	Type    string          `json:"type"`
	Args    json.RawMessage `json:"args"`
	Expires int64           `json:"expires"`
}

// netLabel summarises "which network was this": transport plus the ISP's AS name, so that
// measurements can be grouped per access network (e.g. "cellular/MTS-AS" vs "wifi/MGTS").
func (s *Server) netLabel(state map[string]any, ip string) string {
	transport := ""
	if nw, ok := state["network"].(map[string]any); ok {
		if def, ok := nw["default"].(map[string]any); ok {
			if ts, ok := def["transports"].([]any); ok && len(ts) > 0 {
				transport, _ = ts[0].(string)
			}
		}
	}
	isp := s.ipc.shortName(ip)
	if transport == "" && isp == "" {
		return ""
	}
	return strings.ToLower(transport) + "/" + isp
}

func validTs(ts float64, now float64) float64 {
	// Accept device clocks within a week; anything else is clamped to server time.
	if ts < now-7*86400 || ts > now+600 {
		return now
	}
	return ts
}

func insertMeasurements(tx *sql.Tx, devID string, ms []measIn, net, ip string, now float64) error {
	for _, m := range ms {
		if m.Kind == "" {
			continue
		}
		data := string(m.Data)
		if !json.Valid(m.Data) {
			data = "{}"
		}
		ok := 0
		if m.Ok {
			ok = 1
		}
		if _, err := tx.Exec(`INSERT INTO measurements(device_id, ts, kind, target, ok, ms, net, ip, data) VALUES(?,?,?,?,?,?,?,?,?)`,
			devID, validTs(m.Ts, now), m.Kind, m.Target, ok, m.Ms, net, ip, data); err != nil {
			return err
		}
	}
	return nil
}

func storeResults(tx *sql.Tx, devID string, rs []resultIn, net, ip string, now float64) error {
	for _, rr := range rs {
		status := "done"
		if !rr.Ok {
			status = "failed"
		}
		res := string(rr.Result)
		if !json.Valid(rr.Result) {
			res = ""
		}
		if _, err := tx.Exec(`UPDATE commands SET status=?, result=?, error=?, done_at=? WHERE id=? AND device_id=?`,
			status, res, rr.Error, int64(now), rr.ID, devID); err != nil {
			return err
		}
		if err := insertMeasurements(tx, devID, rr.Measurements, net, ip, now); err != nil {
			return err
		}
	}
	return nil
}

// takeCommands marks queued commands as sent and returns them.
func takeCommands(tx *sql.Tx, devID string, now int64) ([]cmdOut, error) {
	rows, err := tx.Query(`SELECT id, type, args, expires FROM commands WHERE device_id=? AND status='queued' AND expires>? ORDER BY id`, devID, now)
	if err != nil {
		return nil, err
	}
	out := []cmdOut{}
	for rows.Next() {
		var c cmdOut
		var args string
		if err := rows.Scan(&c.ID, &c.Type, &args, &c.Expires); err != nil {
			rows.Close()
			return nil, err
		}
		c.Args = rawOrEmpty(args)
		out = append(out, c)
	}
	rows.Close()
	for _, c := range out {
		if _, err := tx.Exec(`UPDATE commands SET status='sent', sent_at=? WHERE id=?`, now, c.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request, d *Device) {
	var req syncReq
	if err := readJSON(r, 4<<20, &req); err != nil {
		writeErr(w, err)
		return
	}
	ip := clientIP(r)
	s.ipc.lookup(ip)
	nowT := time.Now()
	now := float64(nowT.UnixNano()) / 1e9

	// Sections the device omitted (settings, servers, ... unchanged since the last full upload)
	// are carried over from the previous snapshot.
	last, _ := parseObject(d.LastState)
	merged := last
	if merged == nil {
		merged = map[string]any{}
	}
	for k, v := range req.State {
		merged[k] = v
	}
	merged["server_ts"] = nowT.Unix()
	merged["ip"] = ip
	if info := s.ipc.get(ip); info != nil {
		merged["ip_info"] = info
	}
	net := s.netLabel(merged, ip)
	merged["net_label"] = net
	vpn := 0
	if v, ok := merged["vpn"].(map[string]any); ok {
		if run, _ := v["running"].(bool); run {
			vpn = 1
		}
	}
	mergedJSON, _ := json.Marshal(merged)
	partialJSON, _ := json.Marshal(req.State)
	info := string(req.Device)
	if !json.Valid(req.Device) {
		info = d.Info
	}
	name := d.Name
	if name == "" && req.Name != "" {
		name = req.Name
	}

	var resp map[string]any
	err := s.st.Write(func(tx *sql.Tx) error {
		applied := d.AppliedVersion
		if req.AppliedVersion != "" {
			applied = req.AppliedVersion
		}
		applyRes := d.ApplyResult
		if len(req.ApplyResult) > 0 && json.Valid(req.ApplyResult) && string(req.ApplyResult) != "null" {
			applyRes = string(req.ApplyResult)
			if _, err := tx.Exec(`INSERT INTO events(device_id, ts, type, data) VALUES(?,?,?,?)`, d.ID, now, "config_applied", applyRes); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE devices SET last_seen=?, last_ip=?, info=?, name=?, applied_version=?, apply_result=?, last_state=? WHERE id=?`,
			nowT.Unix(), ip, info, name, applied, applyRes, string(mergedJSON), d.ID); err != nil {
			return err
		}
		if len(req.State) > 0 {
			if _, err := tx.Exec(`INSERT INTO states(device_id, ts, ip, net, vpn, state) VALUES(?,?,?,?,?,?)`,
				d.ID, nowT.Unix(), ip, net, vpn, string(partialJSON)); err != nil {
				return err
			}
		}
		for _, e := range req.Events {
			if e.Type == "" {
				continue
			}
			data := string(e.Data)
			if !json.Valid(e.Data) {
				data = "{}"
			}
			if _, err := tx.Exec(`INSERT INTO events(device_id, ts, type, data) VALUES(?,?,?,?)`, d.ID, validTs(e.Ts, now), e.Type, data); err != nil {
				return err
			}
		}
		if err := insertMeasurements(tx, d.ID, req.Measurements, net, ip, now); err != nil {
			return err
		}
		if err := storeResults(tx, d.ID, req.Results, net, ip, now); err != nil {
			return err
		}

		d.Name = name
		resp = map[string]any{"status": d.Status, "server_time": nowT.Unix(), "device_id": d.ID, "name": name}
		if d.Status != "approved" {
			resp["agent"] = agentParams(nil, d)
			return nil
		}
		eff, err := s.effectiveConfig(tx, d)
		if err != nil {
			return err
		}
		resp["agent"] = agentParams(eff.Telemetry, d)
		resp["config_version"] = eff.Version
		if applied != eff.Version {
			resp["config"] = map[string]any{"version": eff.Version, "doc": eff.Doc}
		}
		cmds, err := takeCommands(tx, d.ID, nowT.Unix())
		if err != nil {
			return err
		}
		resp["commands"] = cmds
		return nil
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleResults(w http.ResponseWriter, r *http.Request, d *Device) {
	var req struct {
		Results []resultIn `json:"results"`
	}
	if err := readJSON(r, 8<<20, &req); err != nil {
		writeErr(w, err)
		return
	}
	ip := clientIP(r)
	last, _ := parseObject(d.LastState)
	net, _ := last["net_label"].(string)
	now := float64(time.Now().UnixNano()) / 1e9
	err := s.st.Write(func(tx *sql.Tx) error { return storeResults(tx, d.ID, req.Results, net, ip, now) })
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

type logLine struct {
	Ts  float64 `json:"ts"`
	Pid int     `json:"pid"`
	Lvl string  `json:"lvl"`
	Tag string  `json:"tag"`
	Msg string  `json:"msg"`
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request, d *Device) {
	var req struct {
		Lines   []logLine `json:"lines"`
		Dropped int       `json:"dropped"`
		Source  string    `json:"source"`
	}
	if err := readJSON(r, 16<<20, &req); err != nil {
		writeErr(w, err)
		return
	}
	now := float64(time.Now().UnixNano()) / 1e9
	maxTs := d.LastLogTs
	err := s.st.Write(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`INSERT INTO logs(device_id, ts, pid, lvl, tag, msg) VALUES(?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, l := range req.Lines {
			ts := validTs(l.Ts, now)
			if len(l.Msg) > 16000 {
				l.Msg = l.Msg[:16000] + "…"
			}
			if _, err := stmt.Exec(d.ID, ts, l.Pid, l.Lvl, l.Tag, l.Msg); err != nil {
				return err
			}
			if ts > maxTs {
				maxTs = ts
			}
			if l.Tag == "AndroidRuntime" && strings.Contains(l.Msg, "FATAL EXCEPTION") {
				data, _ := json.Marshal(map[string]any{"msg": l.Msg, "pid": l.Pid})
				if _, err := tx.Exec(`INSERT INTO events(device_id, ts, type, data) VALUES(?,?,?,?)`, d.ID, ts, "crash", string(data)); err != nil {
					return err
				}
			}
		}
		if req.Dropped > 0 {
			data, _ := json.Marshal(map[string]any{"dropped": req.Dropped, "source": req.Source})
			if _, err := tx.Exec(`INSERT INTO events(device_id, ts, type, data) VALUES(?,?,?,?)`, d.ID, now, "logs_dropped", string(data)); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`UPDATE devices SET last_log_ts=? WHERE id=?`, maxTs, d.ID)
		return err
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "accepted": len(req.Lines)})
}

// handlePoll is the live-mode long poll: it returns as soon as a command is queued or the
// device's configuration changes, or after ?wait seconds.
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request, d *Device) {
	wait := intParam(r, "wait", 50, 290)
	applied := r.URL.Query().Get("v")
	deadline := time.After(time.Duration(wait) * time.Second)
	for {
		ch := s.notify.wait(d.ID)
		var resp map[string]any
		err := s.st.Write(func(tx *sql.Tx) error {
			cur, err := scanDevice(tx.QueryRow(`SELECT `+deviceCols+` FROM devices WHERE id=?`, d.ID))
			if err != nil {
				return err
			}
			if cur.Status != "approved" {
				resp = map[string]any{"status": cur.Status}
				return nil
			}
			eff, err := s.effectiveConfig(tx, cur)
			if err != nil {
				return err
			}
			cmds, err := takeCommands(tx, d.ID, time.Now().Unix())
			if err != nil {
				return err
			}
			changed := applied != "" && applied != eff.Version
			if len(cmds) > 0 || changed {
				resp = map[string]any{"status": cur.Status, "commands": cmds, "config_changed": changed,
					"agent": agentParams(eff.Telemetry, cur)}
			}
			return nil
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		if resp != nil {
			writeJSON(w, 200, resp)
			return
		}
		select {
		case <-ch:
		case <-deadline:
			writeJSON(w, 200, map[string]any{"status": "approved", "commands": []cmdOut{}, "config_changed": false})
			return
		case <-r.Context().Done():
			return
		}
	}
}
