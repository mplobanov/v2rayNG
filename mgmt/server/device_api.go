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

// Delivery is at-least-once for devices that acknowledge commands (deviceAcks): a command handed
// to such an app (awaits_ack) but neither acknowledged (POST /v1/ack, sent when the phone starts
// it) nor answered within redeliverAfter seconds goes out again with the next sync or poll, at
// most maxDeliveries times in all. The response carrying it may never have reached the phone (a
// long poll cut by a VPN restart or a network switch), or the phone dropped it unstarted when its
// agent stopped. The phone skips ids it already started, so a repeat is not executed twice. A
// command handed to an older app stays delivered once, also after that app is upgraded.
var redeliverAfter int64 = 90

const maxDeliveries = 5

// deviceAcks tells whether the device's app acknowledges commands and ignores repeats
// (device info "mgmt_caps" contains "ack"); older apps get every command exactly once.
func deviceAcks(info string) bool {
	var v struct {
		Caps []string `json:"mgmt_caps"`
	}
	if json.Unmarshal([]byte(info), &v) != nil {
		return false
	}
	for _, c := range v.Caps {
		if c == "ack" {
			return true
		}
	}
	return false
}

// takeCommands marks queued commands (and, with redeliver, unacknowledged ones due for
// redelivery) as sent and returns them.
func takeCommands(tx *sql.Tx, devID string, now int64, redeliver bool) ([]cmdOut, error) {
	q := `SELECT id, type, args, expires FROM commands WHERE device_id=? AND expires>? AND (status='queued'`
	qargs := []any{devID, now}
	if redeliver {
		q += ` OR (status='sent' AND awaits_ack=1 AND sent_at<=? AND deliveries<?)`
		qargs = append(qargs, now-redeliverAfter, maxDeliveries)
	}
	rows, err := tx.Query(q+`) ORDER BY id`, qargs...)
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
		if _, err := tx.Exec(`UPDATE commands SET status='sent', sent_at=?, deliveries=deliveries+1, awaits_ack=? WHERE id=?`,
			now, redeliver, c.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// nextRedelivery returns when the device's earliest unacknowledged command becomes due for
// redelivery (unix seconds), or 0 if there is none.
func nextRedelivery(tx *sql.Tx, devID string, now int64) (int64, error) {
	var sent sql.NullInt64
	err := tx.QueryRow(`SELECT MIN(sent_at) FROM commands WHERE device_id=? AND status='sent' AND awaits_ack=1 AND deliveries<? AND expires>?`,
		devID, maxDeliveries, now).Scan(&sent)
	if err != nil || !sent.Valid {
		return 0, err
	}
	return sent.Int64 + redeliverAfter, nil
}

// ackCommands records that the device started the given commands: they are not redelivered.
func ackCommands(tx *sql.Tx, devID string, ids []int64) error {
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE commands SET status='running' WHERE id=? AND device_id=? AND status='sent'`, id, devID); err != nil {
			return err
		}
	}
	return nil
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
			resp["agent"] = s.agentParams(nil, d)
			return nil
		}
		eff, err := s.effectiveConfig(tx, d)
		if err != nil {
			return err
		}
		resp["agent"] = s.agentParams(eff.Telemetry, d)
		resp["config_version"] = eff.Version
		if applied != eff.Version {
			resp["config"] = map[string]any{"version": eff.Version, "doc": eff.Doc}
		}
		cmds, err := takeCommands(tx, d.ID, nowT.Unix(), deviceAcks(info))
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

// handleAck takes the ids of commands the device has started (see takeCommands).
func (s *Server) handleAck(w http.ResponseWriter, r *http.Request, d *Device) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := readJSON(r, 64<<10, &req); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.st.Write(func(tx *sql.Tx) error { return ackCommands(tx, d.ID, req.IDs) }); err != nil {
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

// coreLevel returns the level of a log line, taking it from xray's own "[Error] …" prefix for
// core (GoLog) lines: libv2ray writes everything to logcat as I, which would hide core errors
// from level filters and the timeline. Routine noise is demoted: the cancelled loser of a
// parallel DoQ+DoH lookup, and the start banner and deprecation notice every core (including the
// temporary ones real_ping starts) prints.
func coreLevel(l logLine) string {
	if l.Tag != "GoLog" {
		return l.Lvl
	}
	switch {
	case strings.HasPrefix(l.Msg, "[Error]"):
		if strings.Contains(l.Msg, " app/dns: ") && strings.HasSuffix(strings.TrimSpace(l.Msg), "context canceled") {
			return "D"
		}
		return "E"
	case strings.HasPrefix(l.Msg, "[Warning]"):
		if strings.HasPrefix(l.Msg, "[Warning] core: Xray ") || strings.Contains(l.Msg, " is deprecated") {
			return "I"
		}
		return "W"
	case strings.HasPrefix(l.Msg, "[Debug]"):
		return "D"
	}
	return l.Lvl
}

// dnsLogRe matches the line xray's "log.dnsLog" writes per upstream DNS query:
// "<server> got answer: <domain> -> [<ips>] <elapsed> <<error>>" (cache hits say "cache HIT:").
// dnsTimeoutMs: xray gives each resolver 4 s per query (timeoutMs) and logs a resolver that did
// not answer in time as "record not found" at that elapsed time, so such a line is a timeout.
const dnsTimeoutMs = 3900

var dnsLogRe = regexp.MustCompile(`^(\S+) got answer: (\S+) -> \[([^\]]*)\](?: (\S+))?(?: <(.*)>)?$`)

// dnsMeasurement turns a core dnsLog line into a measurement of the resolver that answered:
// kind "dns" (ok = the server answered, "no such record" included), or "dns_canceled" for the
// loser of a parallel query, which tells how often a resolver was slower than the other.
func dnsMeasurement(l logLine) (measIn, bool) {
	if l.Tag != "GoLog" {
		return measIn{}, false
	}
	m := dnsLogRe.FindStringSubmatch(l.Msg)
	if m == nil {
		return measIn{}, false
	}
	server, domain, ips, elapsed, errText := m[1], m[2], m[3], m[4], m[5]
	out := measIn{Ts: l.Ts, Kind: "dns", Target: server, Ms: -1}
	if d, err := time.ParseDuration(elapsed); err == nil {
		out.Ms = float64(d.Microseconds()) / 1000
	}
	n := 0
	if ips != "" {
		n = len(strings.Split(ips, ", "))
	}
	data := map[string]any{"domain": strings.TrimSuffix(domain, "."), "ips": n}
	if n > 0 && strings.Trim(strings.NewReplacer("0.0.0.0", "", "::", "", ",", "", " ", "").Replace(ips), "") == "" {
		data["blocked"] = true // the resolver's filter (AdGuard Home answers 0.0.0.0 / ::)
	}
	switch {
	case errText == "":
		out.Ok = true
	case strings.Contains(errText, "context canceled"):
		out.Kind = "dns_canceled"
	case (strings.Contains(errText, "record not found") || strings.Contains(errText, "empty response")) && out.Ms >= dnsTimeoutMs:
		data["error"] = "timeout: no answer within the resolver's 4 s"
	case strings.Contains(errText, "record not found") || strings.Contains(errText, "empty response") ||
		strings.Contains(errText, "rcode: 3"):
		out.Ok = true
		data["result"] = "no record"
	default:
		data["error"] = errText
	}
	out.Data, _ = json.Marshal(data)
	return out, true
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
	last, _ := parseObject(d.LastState)
	net, _ := last["net_label"].(string)
	ip, _ := last["ip"].(string)
	var dnsMs []measIn
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
			if _, err := stmt.Exec(d.ID, ts, l.Pid, coreLevel(l), l.Tag, l.Msg); err != nil {
				return err
			}
			if ts > maxTs {
				maxTs = ts
			}
			if m, ok := dnsMeasurement(l); ok {
				dnsMs = append(dnsMs, m)
			}
			if l.Tag == "AndroidRuntime" && strings.Contains(l.Msg, "FATAL EXCEPTION") {
				data, _ := json.Marshal(map[string]any{"msg": l.Msg, "pid": l.Pid})
				if _, err := tx.Exec(`INSERT INTO events(device_id, ts, type, data) VALUES(?,?,?,?)`, d.ID, ts, "crash", string(data)); err != nil {
					return err
				}
			}
		}
		if err := insertMeasurements(tx, d.ID, dnsMs, net, ip, now); err != nil {
			return err
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

// handlePoll is the live-mode long poll: it returns as soon as a command is queued (or due for
// redelivery) or the device's configuration changes, or after ?wait seconds.
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request, d *Device) {
	wait := intParam(r, "wait", 50, 290)
	applied := r.URL.Query().Get("v")
	deadline := time.After(time.Duration(wait) * time.Second)
	for {
		ch := s.notify.wait(d.ID)
		var resp map[string]any
		var redeliverAt int64
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
			now := time.Now().Unix()
			acks := deviceAcks(cur.Info)
			cmds, err := takeCommands(tx, d.ID, now, acks)
			if err != nil {
				return err
			}
			changed := applied != "" && applied != eff.Version
			if len(cmds) > 0 || changed {
				resp = map[string]any{"status": cur.Status, "commands": cmds, "config_changed": changed,
					"agent": s.agentParams(eff.Telemetry, cur)}
				return nil
			}
			if acks {
				redeliverAt, err = nextRedelivery(tx, d.ID, now)
			}
			return err
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		if resp != nil {
			writeJSON(w, 200, resp)
			return
		}
		var redeliver <-chan time.Time
		if redeliverAt > 0 {
			redeliver = time.After(time.Until(time.Unix(redeliverAt, 0)))
		}
		select {
		case <-ch:
		case <-redeliver:
		case <-deadline:
			writeJSON(w, 200, map[string]any{"status": "approved", "commands": []cmdOut{}, "config_changed": false})
			return
		case <-r.Context().Done():
			return
		}
	}
}
