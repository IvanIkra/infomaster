// Package store is the SQLite storage (§9). Tables arrive with the stages that fill them; `create table if not
// exists` adds them to a database of an earlier stage, and upgrades add the columns a later stage gave to an
// existing table. The schema is the Python service's, so both read the same database file.
package store

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	_ "modernc.org/sqlite" // pure Go: the image needs no cgo
)

//go:embed schema.sql
var schema string

// Store is one connection, as in the Python service: writes are serialized, and SQLite never sees two writers.
type Store struct {
	DB *sql.DB
}

// Open opens (and creates) the database at path; ":memory:" gives a database for tests.
func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one connection: ":memory:" stays one database, and writes never race
	s := &Store{DB: db}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	if err := s.upgrade(); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade: %w", err)
	}
	return s, nil
}

func (s *Store) upgrade() error {
	for _, u := range upgrades {
		table, column, definition := u[0], u[1], u[2]
		rows, err := s.DB.Query("select name from pragma_table_info(?)", table)
		if err != nil {
			return err
		}
		found := false
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			if name == column {
				found = true
			}
		}
		rows.Close()
		if !found {
			if _, err := s.DB.Exec(fmt.Sprintf("alter table %s add column %s %s", table, column, definition)); err != nil {
				return err
			}
		}
	}
	return nil
}

// GetJSON reads a kv value into out; it reports false when the key is missing.
func (s *Store) GetJSON(key string, out any) (bool, error) {
	var value string
	err := s.DB.QueryRow("select value from kv where key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, pyjson.Unmarshal([]byte(value), out)
}

// PutJSON writes a kv value as Python's json.dumps(ensure_ascii=False) would.
func (s *Store) PutJSON(key string, value any) error {
	text, err := pyjson.Dumps(value)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec("insert into kv(key, value) values(?, ?) on conflict(key) do update set value = excluded.value",
		key, text)
	return err
}

// DeleteJSON removes a kv key.
func (s *Store) DeleteJSON(key string) error {
	_, err := s.DB.Exec("delete from kv where key = ?", key)
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.DB.Close() }

// Querier is what both *sql.DB and *sql.Tx give: a repository bound to either runs inside or outside a caller's
// transaction ("with store.db" in the Python service).
type Querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// InTx runs fn in one transaction: committed when fn returns nil, rolled back otherwise. The store has one
// connection, so nothing may use s.DB while fn runs: bind repositories to tx with their With(tx).
func (s *Store) InTx(fn func(tx *sql.Tx) error) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
