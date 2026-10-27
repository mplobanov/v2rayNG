package main

import (
	"database/sql"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database. SQLite allows one writer at a time; wmu serialises
// writers inside this process so busy-retries never surface as "database is locked".
type Store struct {
	db  *sql.DB
	wmu sync.Mutex
}

const schema = `
CREATE TABLE IF NOT EXISTS devices (
  id TEXT PRIMARY KEY,
  token_hash TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  profile TEXT NOT NULL DEFAULT 'default',
  overrides TEXT NOT NULL DEFAULT '{}',
  note TEXT NOT NULL DEFAULT '',
  enrolled_at INTEGER NOT NULL,
  last_seen INTEGER NOT NULL DEFAULT 0,
  last_ip TEXT NOT NULL DEFAULT '',
  info TEXT NOT NULL DEFAULT '{}',
  applied_version TEXT NOT NULL DEFAULT '',
  apply_result TEXT NOT NULL DEFAULT '{}',
  live_until INTEGER NOT NULL DEFAULT 0,
  loglevel TEXT NOT NULL DEFAULT '',
  loglevel_until INTEGER NOT NULL DEFAULT 0,
  last_state TEXT NOT NULL DEFAULT '{}',
  last_log_ts REAL NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS profiles (
  name TEXT PRIMARY KEY,
  doc TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS audit (
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  target TEXT NOT NULL,
  action TEXT NOT NULL,
  before TEXT NOT NULL DEFAULT '',
  after TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS states (
  id INTEGER PRIMARY KEY,
  device_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  ip TEXT NOT NULL DEFAULT '',
  net TEXT NOT NULL DEFAULT '',
  vpn INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS states_dev_ts ON states(device_id, ts);
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY,
  device_id TEXT NOT NULL,
  ts REAL NOT NULL,
  type TEXT NOT NULL,
  data TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS events_dev_ts ON events(device_id, ts);
CREATE INDEX IF NOT EXISTS events_ts ON events(ts);
CREATE TABLE IF NOT EXISTS measurements (
  id INTEGER PRIMARY KEY,
  device_id TEXT NOT NULL,
  ts REAL NOT NULL,
  kind TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  ok INTEGER NOT NULL,
  ms REAL NOT NULL DEFAULT -1,
  net TEXT NOT NULL DEFAULT '',
  ip TEXT NOT NULL DEFAULT '',
  data TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS meas_dev_ts ON measurements(device_id, ts);
CREATE INDEX IF NOT EXISTS meas_ts ON measurements(ts);
CREATE TABLE IF NOT EXISTS logs (
  id INTEGER PRIMARY KEY,
  device_id TEXT NOT NULL,
  ts REAL NOT NULL,
  pid INTEGER NOT NULL DEFAULT 0,
  lvl TEXT NOT NULL DEFAULT '',
  tag TEXT NOT NULL DEFAULT '',
  msg TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS logs_dev_ts ON logs(device_id, ts);
CREATE INDEX IF NOT EXISTS logs_ts ON logs(ts);
CREATE TABLE IF NOT EXISTS commands (
  id INTEGER PRIMARY KEY,
  device_id TEXT NOT NULL,
  created INTEGER NOT NULL,
  expires INTEGER NOT NULL,
  type TEXT NOT NULL,
  args TEXT NOT NULL DEFAULT '{}',
  status TEXT NOT NULL DEFAULT 'queued',
  sent_at INTEGER NOT NULL DEFAULT 0,
  done_at INTEGER NOT NULL DEFAULT 0,
  result TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS cmd_dev_status ON commands(device_id, status);
CREATE TABLE IF NOT EXISTS ipinfo (
  ip TEXT PRIMARY KEY,
  ts INTEGER NOT NULL,
  data TEXT NOT NULL
);
`

func OpenStore(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// migrate applies one-off data migrations, tracked in PRAGMA user_version.
func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v < 1 {
		// 1: core lines stored before coreLevel existed carry logcat's I; give them xray's level.
		err := s.Write(func(tx *sql.Tx) error {
			rows, err := tx.Query(`SELECT id, lvl, msg FROM logs WHERE tag='GoLog' AND msg LIKE '[%'`)
			if err != nil {
				return err
			}
			fix := map[int64]string{}
			for rows.Next() {
				var id int64
				var l logLine
				if err := rows.Scan(&id, &l.Lvl, &l.Msg); err != nil {
					rows.Close()
					return err
				}
				l.Tag = "GoLog"
				if lvl := coreLevel(l); lvl != l.Lvl {
					fix[id] = lvl
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for id, lvl := range fix {
				if _, err := tx.Exec(`UPDATE logs SET lvl=? WHERE id=?`, lvl, id); err != nil {
					return err
				}
			}
			_, err = tx.Exec(`PRAGMA user_version=1`)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Write runs fn in a transaction while holding the process-wide writer lock.
func (s *Store) Write(fn func(tx *sql.Tx) error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) Exec(q string, args ...any) (sql.Result, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.db.Exec(q, args...)
}
