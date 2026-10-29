// Copyright 2026 The Sqlite Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sqlite3_test

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"unsafe"

	"modernc.org/libc/sys/stat"
	libctime "modernc.org/libc/time"
	_ "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestSQLiteLinuxPPCLayouts(t *testing.T) {
	// Adélie musl 1.2.3-r5's time64 layouts, measured by its own compiler:
	// runs/libc-linuxppc-20261029/capture/layout.out in the port workspace.
	for _, tt := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"engine stat", unsafe.Sizeof(sqlite3.Tstat{}), 152},
		{"libc stat", unsafe.Sizeof(stat.Stat{}), 152},
		{"engine timespec", unsafe.Sizeof(sqlite3.Ttimespec{}), 16},
		{"libc timespec", unsafe.Sizeof(libctime.Timespec{}), 16},
		{"engine timespec.tv_nsec", unsafe.Offsetof(sqlite3.Ttimespec{}.Ftv_nsec), 12},
		{"libc timespec.tv_nsec", unsafe.Offsetof(libctime.Timespec{}.Ftv_nsec), 12},
	} {
		if tt.got != tt.want {
			t.Errorf("%s: got %d, want %d", tt.name, tt.got, tt.want)
		}
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(sqlite3.Tstat{}), reflect.TypeOf(stat.Stat{})} {
		for _, field := range []struct {
			name   string
			offset uintptr
		}{
			{"Fst_dev", 0}, {"Fst_ino", 8}, {"Fst_mode", 16}, {"Fst_rdev", 32},
			{"Fst_size", 48}, {"Fst_blocks", 64},
			{"Fst_atim", 104}, {"Fst_mtim", 120}, {"Fst_ctim", 136},
		} {
			got, ok := typ.FieldByName(field.name)
			if !ok || got.Offset != field.offset {
				t.Errorf("%v.%s: got offset %d (present %v), want %d", typ, field.name, got.Offset, ok, field.offset)
			}
		}
	}
}

func TestSQLiteLinuxPPCRoundTripWAL(t *testing.T) {
	name := filepath.Join(t.TempDir(), "linux-ppc.db")
	db, err := sql.Open("sqlite", name)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		"CREATE TABLE roundtrip (id INTEGER PRIMARY KEY, value TEXT NOT NULL)",
		"INSERT INTO roundtrip VALUES (1, 'before WAL')",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	var value string
	if err := db.QueryRow("SELECT value FROM roundtrip WHERE id=1").Scan(&value); err != nil || value != "before WAL" {
		t.Fatalf("initial SELECT = %q, %v", value, err)
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
	if _, err := db.Exec("INSERT INTO roundtrip VALUES (2, 'in WAL')"); err != nil {
		t.Fatal(err)
	}
	// A second connection must see the committed WAL data, not merely a
	// cached value in the writer's connection.
	reader, err := sql.Open("sqlite", name)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := reader.QueryRow("SELECT value FROM roundtrip WHERE id=2").Scan(&value); err != nil || value != "in WAL" {
		t.Fatalf("WAL SELECT = %q, %v", value, err)
	}
	if _, err := db.Exec("INSERT INTO missing_table VALUES (1)"); err == nil {
		t.Fatal("invalid statement succeeded")
	}
	t.Log("CREATE/INSERT/SELECT, journal_mode=wal, WAL INSERT/second-connection SELECT, and invalid-statement error passed")
}
