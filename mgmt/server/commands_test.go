package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.db.Close() })
	return st
}

func queueCommand(t *testing.T, st *Store, dev, typ string, created, expires int64) int64 {
	t.Helper()
	res, err := st.Exec(`INSERT INTO commands(device_id, created, expires, type, args) VALUES(?,?,?,?,'{}')`, dev, created, expires, typ)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func take(t *testing.T, st *Store, dev string, now int64, redeliver bool) []int64 {
	t.Helper()
	var ids []int64
	err := st.Write(func(tx *sql.Tx) error {
		cmds, err := takeCommands(tx, dev, now, redeliver)
		for _, c := range cmds {
			ids = append(ids, c.ID)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func commandRow(t *testing.T, st *Store, id int64) (status string, deliveries int) {
	t.Helper()
	if err := st.db.QueryRow(`SELECT status, deliveries FROM commands WHERE id=?`, id).Scan(&status, &deliveries); err != nil {
		t.Fatal(err)
	}
	return
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTakeCommandsRedeliversUnacknowledged(t *testing.T) {
	st := openTestStore(t)
	a := queueCommand(t, st, "d", "select_server", 1000, 5000)
	b := queueCommand(t, st, "d", "update_subscriptions", 1000, 5000)
	other := queueCommand(t, st, "e", "sync", 1000, 5000)

	if got := take(t, st, "d", 1000, true); !sameIDs(got, []int64{a, b}) {
		t.Fatalf("first delivery %v, want [%d %d]", got, a, b)
	}
	if got := take(t, st, "d", 1000+redeliverAfter-1, true); len(got) != 0 {
		t.Fatalf("redelivered before %d s: %v", redeliverAfter, got)
	}

	// The phone started a; b never arrived.
	if err := st.Write(func(tx *sql.Tx) error { return ackCommands(tx, "d", []int64{a}) }); err != nil {
		t.Fatal(err)
	}
	if got := take(t, st, "d", 1000+redeliverAfter, true); !sameIDs(got, []int64{b}) {
		t.Fatalf("redelivery %v, want [%d]", got, b)
	}
	if s, n := commandRow(t, st, a); s != "running" || n != 1 {
		t.Errorf("acknowledged command: %s after %d deliveries, want running after 1", s, n)
	}
	if s, n := commandRow(t, st, b); s != "sent" || n != 2 {
		t.Errorf("redelivered command: %s after %d deliveries, want sent after 2", s, n)
	}
	if s, _ := commandRow(t, st, other); s != "queued" {
		t.Errorf("another device's command is %s", s)
	}

	// A result settles the command whether or not an ack came first.
	if err := st.Write(func(tx *sql.Tx) error {
		return storeResults(tx, "d", []resultIn{{ID: a, Ok: true, Result: json.RawMessage(`{}`)}, {ID: b, Ok: true}}, "", "", 1200)
	}); err != nil {
		t.Fatal(err)
	}
	if got := take(t, st, "d", 4000, true); len(got) != 0 {
		t.Fatalf("answered commands redelivered: %v", got)
	}
	// An ack arriving after the result does not reopen it.
	st.Write(func(tx *sql.Tx) error { return ackCommands(tx, "d", []int64{a}) })
	if s, _ := commandRow(t, st, a); s != "done" {
		t.Errorf("late ack turned a done command into %s", s)
	}
}

func TestTakeCommandsRedeliveryLimits(t *testing.T) {
	st := openTestStore(t)
	id := queueCommand(t, st, "d", "probe", 1000, 100000)
	now := int64(1000)
	for i := 1; i <= maxDeliveries; i++ {
		if got := take(t, st, "d", now, true); !sameIDs(got, []int64{id}) {
			t.Fatalf("delivery %d: %v", i, got)
		}
		now += redeliverAfter
	}
	if got := take(t, st, "d", now+3600, true); len(got) != 0 {
		t.Fatalf("delivered more than %d times: %v", maxDeliveries, got)
	}
	if s, n := commandRow(t, st, id); s != "sent" || n != maxDeliveries {
		t.Errorf("%s after %d deliveries", s, n)
	}

	// Expired commands are never redelivered.
	queueCommand(t, st, "d", "sync", 1000, 1100)
	take(t, st, "d", 1000, true)
	if got := take(t, st, "d", 1100+redeliverAfter, true); len(got) != 0 {
		t.Fatalf("expired command redelivered: %v", got)
	}
	// Apps that do not acknowledge get each command once.
	once := queueCommand(t, st, "d", "sync", 1000, 100000)
	if got := take(t, st, "d", 5000, false); !sameIDs(got, []int64{once}) {
		t.Fatalf("legacy delivery %v", got)
	}
	if got := take(t, st, "d", 9000, false); len(got) != 0 {
		t.Fatalf("legacy device got a repeat: %v", got)
	}
	// ... also once the app is upgraded: the old app may have dropped it, or run it without a
	// result, and running it late would surprise (a stale select_server overriding a newer choice).
	if got := take(t, st, "d", 9500, true); len(got) != 0 {
		t.Fatalf("command handed to the old app redelivered after the upgrade: %v", got)
	}
	var next int64
	st.Write(func(tx *sql.Tx) (err error) { next, err = nextRedelivery(tx, "d", 9500); return })
	if next != 0 {
		t.Errorf("poll would wake for a pre-upgrade command at %d", next)
	}
}

func TestNextRedelivery(t *testing.T) {
	st := openTestStore(t)
	next := func(now int64) int64 {
		var at int64
		if err := st.Write(func(tx *sql.Tx) (err error) { at, err = nextRedelivery(tx, "d", now); return }); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if at := next(1000); at != 0 {
		t.Fatalf("nothing sent, next %d", at)
	}
	queueCommand(t, st, "d", "sync", 1000, 5000)
	queueCommand(t, st, "d", "sync", 1000, 5000)
	take(t, st, "d", 1010, true)
	if at := next(1020); at != 1010+redeliverAfter {
		t.Fatalf("next %d, want %d", at, 1010+redeliverAfter)
	}
	st.Write(func(tx *sql.Tx) error { return ackCommands(tx, "d", []int64{1, 2}) })
	if at := next(1020); at != 0 {
		t.Fatalf("acknowledged commands still due at %d", at)
	}
}

func TestDeviceAcks(t *testing.T) {
	cases := map[string]bool{
		`{"app_code":756,"mgmt_caps":["ack"]}`: true,
		`{"mgmt_caps":["other","ack"]}`:        true,
		`{"app_code":755}`:                     false,
		`{"mgmt_caps":"ack"}`:                  false,
		`{}`:                                   false,
		``:                                     false,
	}
	for info, want := range cases {
		if got := deviceAcks(info); got != want {
			t.Errorf("deviceAcks(%q) = %v, want %v", info, got, want)
		}
	}
}

func TestMigrateCommandDeliveries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The commands table as schema version 2 created it.
	for _, q := range []string{
		`CREATE TABLE commands (id INTEGER PRIMARY KEY, device_id TEXT NOT NULL, created INTEGER NOT NULL,
			expires INTEGER NOT NULL, type TEXT NOT NULL, args TEXT NOT NULL DEFAULT '{}',
			status TEXT NOT NULL DEFAULT 'queued', sent_at INTEGER NOT NULL DEFAULT 0,
			done_at INTEGER NOT NULL DEFAULT 0, result TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO commands(device_id, created, expires, type, status, sent_at) VALUES('d', 1, 100000, 'probe', 'sent', 1)`,
		`INSERT INTO commands(device_id, created, expires, type) VALUES('d', 1, 100000, 'sync')`,
		`PRAGMA user_version=2`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	if _, n := commandRow(t, st, 1); n != 1 {
		t.Errorf("sent command: deliveries %d, want 1", n)
	}
	if _, n := commandRow(t, st, 2); n != 0 {
		t.Errorf("queued command: deliveries %d, want 0", n)
	}
	var v int
	st.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != 4 {
		t.Errorf("user_version %d, want 4", v)
	}
	// A command an older server handed out is never redelivered; a queued one still goes out.
	if got := take(t, st, "d", 1000, true); !sameIDs(got, []int64{2}) {
		t.Errorf("after migration delivered %v, want [2]", got)
	}
}

// pollIDs long-polls as the device and returns the command ids it got (nil on a timeout).
func pollIDs(t *testing.T, url, token string, wait time.Duration) []int64 {
	t.Helper()
	req, _ := http.NewRequest("GET", url+"/v1/poll?wait=60", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	c := &http.Client{Timeout: wait}
	resp, err := c.Do(req)
	if err != nil {
		return nil // client timeout: nothing delivered
	}
	defer resp.Body.Close()
	var body struct {
		Commands []cmdOut `json:"commands"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, c := range body.Commands {
		ids = append(ids, c.ID)
	}
	return ids
}

func TestPollRedeliversAfterLostResponse(t *testing.T) {
	saved := redeliverAfter
	redeliverAfter = 1
	t.Cleanup(func() { redeliverAfter = saved })

	st := openTestStore(t)
	srv := NewServer(&Config{}, st)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	now := time.Now().Unix()
	for _, d := range []struct{ id, token, info string }{
		{"dev-acks", "tok-acks", `{"mgmt_caps":["ack"]}`},
		{"dev-legacy", "tok-legacy", `{}`},
	} {
		if _, err := st.Exec(`INSERT INTO devices(id, token_hash, status, enrolled_at, info) VALUES(?,?,'approved',?,?)`,
			d.id, sha256Hex(d.token), now, d.info); err != nil {
			t.Fatal(err)
		}
	}
	id := queueCommand(t, st, "dev-acks", "select_server", now, now+3600)
	legacy := queueCommand(t, st, "dev-legacy", "select_server", now, now+3600)

	if got := pollIDs(t, ts.URL, "tok-acks", 5*time.Second); !sameIDs(got, []int64{id}) {
		t.Fatalf("first poll %v", got)
	}
	// That response "was lost": the next poll gets the command again once it is due, without
	// any notification waking it.
	start := time.Now()
	if got := pollIDs(t, ts.URL, "tok-acks", 5*time.Second); !sameIDs(got, []int64{id}) {
		t.Fatalf("redelivery poll %v", got)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("redelivery took %v", el)
	}

	// The phone acknowledges: no more repeats.
	req, _ := http.NewRequest("POST", ts.URL+"/v1/ack", strings.NewReader(fmt.Sprintf(`{"ids":[%d]}`, id)))
	req.Header.Set("Authorization", "Bearer tok-acks")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("ack: %v %v", resp, err)
	}
	resp.Body.Close()
	if got := pollIDs(t, ts.URL, "tok-acks", 2500*time.Millisecond); got != nil {
		t.Fatalf("acknowledged command redelivered: %v", got)
	}
	if s, _ := commandRow(t, st, id); s != "running" {
		t.Errorf("status %s, want running", s)
	}

	// A device without the capability keeps exactly-once delivery.
	if got := pollIDs(t, ts.URL, "tok-legacy", 5*time.Second); !sameIDs(got, []int64{legacy}) {
		t.Fatalf("legacy first poll %v", got)
	}
	if got := pollIDs(t, ts.URL, "tok-legacy", 2500*time.Millisecond); got != nil {
		t.Fatalf("legacy device got a repeat: %v", got)
	}
}
