package main

import (
	"path/filepath"
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
	if v != 1 {
		t.Errorf("user_version %d, want 1", v)
	}
}
