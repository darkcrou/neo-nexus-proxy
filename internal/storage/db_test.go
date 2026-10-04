package storage

import (
	"path/filepath"
	"testing"
)

func TestWALPragmaApplied(t *testing.T) {
	dir := t.TempDir()
	db, err := New(filepath.Join(dir, "nexus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.conn.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal (DSN pragma not applied)", mode)
	}
	var timeout int
	if err := db.conn.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}
