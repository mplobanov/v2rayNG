package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCoreLevel(t *testing.T) {
	cases := []struct{ tag, lvl, msg, want string }{
		{"GoLog", "I", `[Error] app/dns: failed to retrieve response for x.com. > Post "https://dns.example.net:8443/dns-query": context canceled`, "D"},
		{"GoLog", "I", `[Error] app/dns: failed to retrieve response for x.com. > context deadline exceeded`, "E"},
		{"GoLog", "I", `[Error] proxy/trojan: failed to find an available destination > dial tcp 1.2.3.4:443: i/o timeout`, "E"},
		{"GoLog", "I", `[Warning] core: Xray 26.10.21 started`, "I"},
		{"GoLog", "I", `[Warning] common/errors: The feature Trojan (with no Flow, etc.) is deprecated, not recommended`, "I"},
		{"GoLog", "I", `[Warning] transport/internet/reality: invalid connection`, "W"},
		{"GoLog", "I", `[Debug] app/dns: cache HIT`, "D"},
		{"GoLog", "I", `from tcp:10.10.14.1:37484 accepted tcp:1.2.3.4:443 [tun >> proxy]`, "I"},
		{"v2rayNG", "W", `[Error] not a core line`, "W"},
	}
	for _, c := range cases {
		if got := coreLevel(logLine{Tag: c.tag, Lvl: c.lvl, Msg: c.msg}); got != c.want {
			t.Errorf("coreLevel(%q) = %s, want %s", c.msg, got, c.want)
		}
	}
}

func TestMigrateCoreLevels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// Rows as an older server stored them.
	for _, m := range []string{"[Error] proxy/vless: dial failed", "[Warning] core: Xray 1 started", "plain access line"} {
		if _, err := st.Exec(`INSERT INTO logs(device_id, ts, lvl, tag, msg) VALUES('d', 1, 'I', 'GoLog', ?)`, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Exec(`PRAGMA user_version=0`); err != nil {
		t.Fatal(err)
	}
	st.db.Close()

	st, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	got := map[string]string{}
	rows, err := st.db.Query(`SELECT msg, lvl FROM logs`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var m, l string
		rows.Scan(&m, &l)
		got[m] = l
	}
	rows.Close()
	want := map[string]string{"[Error] proxy/vless: dial failed": "E", "[Warning] core: Xray 1 started": "I", "plain access line": "I"}
	for m, l := range want {
		if got[m] != l {
			t.Errorf("%q: lvl %q, want %q", m, got[m], l)
		}
	}
	var v int
	st.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != 2 {
		t.Errorf("user_version %d, want 2", v)
	}
}

func TestDNSMeasurement(t *testing.T) {
	cases := []struct {
		msg, kind, target string
		ok                bool
		ms                float64
		dataHas           string
	}{
		{"DOH//dns.example.net:8443 got answer: www.youtube.com. -> [142.250.74.46, 142.250.74.78] 41.5ms", "dns", "DOH//dns.example.net:8443", true, 41.5, `"ips":2`},
		{"quic+local://dns.example.net got answer: x.com. -> [] 4.000123s <failed to open quic connection > context deadline exceeded>", "dns", "quic+local://dns.example.net", false, 4000.123, `"error":"failed to open quic connection`},
		{"DOH//dns.example.net:8443 got answer: graph.facebook.com. -> [] 25ms <context canceled>", "dns_canceled", "DOH//dns.example.net:8443", false, 25, `"domain":"graph.facebook.com"`},
		{"UDP:77.88.8.8:53 got answer: nosuch.ya.ru. -> [] 12ms <rcode: 3>", "dns", "UDP:77.88.8.8:53", true, 12, `"result":"no record"`},
		{"UDP:77.88.8.8:53 got answer: ipv6only.ru. -> [] 9µs <record not found>", "dns", "UDP:77.88.8.8:53", true, 0.009, `"result":"no record"`},
		{"quic+local://dns.example.net got answer: rr8.example.com. -> [] 4.000524634s <app/dns: record not found>", "dns", "quic+local://dns.example.net", false, 4000.524, `"error":"timeout`},
		{"quic+local://dns.example.net got answer: ads.example.com. -> [0.0.0.0, ::] 7.2ms", "dns", "quic+local://dns.example.net", true, 7.2, `"blocked":true`},
	}
	for _, c := range cases {
		m, ok := dnsMeasurement(logLine{Tag: "GoLog", Lvl: "I", Ts: 1, Msg: c.msg})
		if !ok {
			t.Fatalf("not parsed: %q", c.msg)
		}
		if m.Kind != c.kind || m.Target != c.target || m.Ok != c.ok || m.Ms != c.ms || !strings.Contains(string(m.Data), c.dataHas) {
			t.Errorf("%q -> %+v data=%s", c.msg, m, m.Data)
		}
	}
	if m, _ := dnsMeasurement(logLine{Tag: "GoLog", Msg: "UDP:1.1.1.1:53 got answer: a.com. -> [10.0.0.1] 1ms"}); strings.Contains(string(m.Data), "blocked") {
		t.Error("a real answer marked blocked")
	}
	for _, msg := range []string{
		"DOH//dns.example.net:8443 cache HIT: www.youtube.com. -> [142.250.74.46]",
		"from tcp:10.10.14.1:37484 accepted tcp:1.2.3.4:443 [tun >> proxy]",
	} {
		if _, ok := dnsMeasurement(logLine{Tag: "GoLog", Msg: msg}); ok {
			t.Errorf("parsed a non-query line: %q", msg)
		}
	}
	if _, ok := dnsMeasurement(logLine{Tag: "v2rayNG", Msg: "UDP:1.1.1.1:53 got answer: a. -> [] 1ms"}); ok {
		t.Error("parsed a non-core line")
	}
}

func TestMigrateDNSTimeouts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO measurements(device_id, ts, kind, target, ok, ms, data) VALUES('d', 1, 'dns', 'q', 1, ?, ?)`
	st.Exec(ins, 4000.5, `{"domain":"a","ips":0,"result":"no record"}`)
	st.Exec(ins, 12.0, `{"domain":"b","ips":0,"result":"no record"}`)
	st.Exec(`PRAGMA user_version=1`)
	st.db.Close()

	st, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	rows, _ := st.db.Query(`SELECT json_extract(data, '$.domain'), ok, data FROM measurements ORDER BY ms`)
	defer rows.Close()
	want := map[string]int{"a": 0, "b": 1}
	for rows.Next() {
		var d, data string
		var ok int
		rows.Scan(&d, &ok, &data)
		if ok != want[d] {
			t.Errorf("%s: ok=%d data=%s", d, ok, data)
		}
		if d == "a" && (!strings.Contains(data, "timeout") || strings.Contains(data, "no record")) {
			t.Errorf("a: data=%s", data)
		}
	}
}
