// Copyright 2026 The Sqlite Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sqlite // import "modernc.org/sqlite"

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// busyTestConns opens the database name in dir with busyTimeout and returns a
// connection holding the lock that lockSQL takes and a second connection, so
// that a write, or with an EXCLUSIVE lock any access, on the second one waits.
// With otherAfterLock the second connection is opened once the lock is held
// and without a journal_mode PRAGMA, which would have loaded the schema: its
// first statement then has to read the schema past the lock.
func busyTestConns(t *testing.T, dir, name, busyTimeout, journalMode, lockSQL string, otherAfterLock bool) (holder, other *sql.Conn) {
	t.Helper()
	dsn := "file:" + filepath.Join(dir, name) + "?_pragma=busy_timeout(" + busyTimeout + ")"
	if journalMode != "" {
		dsn += "&_pragma=journal_mode(" + journalMode + ")"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if holder, err = db.Conn(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close() })
	conn := func() {
		if other, err = db.Conn(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { other.Close() })
	}
	if !otherAfterLock {
		conn()
	}
	if _, err := holder.ExecContext(ctx, lockSQL); err != nil {
		t.Fatal(err)
	}
	if otherAfterLock {
		conn()
	}
	return holder, other
}

// A wait for a lock under busy_timeout ends once the context of the call is
// done, with the context's error, instead of lasting until the timeout. See
// https://gitlab.com/cznic/sqlite/-/issues/262.
func TestBusyTimeoutWaitEndsWithContext(t *testing.T) {
	const busyTimeout = 10 * time.Second
	dir := t.TempDir()
	for _, tc := range []struct {
		name           string
		journalMode    string
		lockSQL        string
		otherAfterLock bool
		call           func(ctx context.Context, c *sql.Conn) error
	}{
		{"ExecContext", "WAL", "BEGIN IMMEDIATE", false, func(ctx context.Context, c *sql.Conn) error {
			_, err := c.ExecContext(ctx, "INSERT INTO t VALUES (1)")
			return err
		}},
		{"QueryContext", "WAL", "BEGIN IMMEDIATE", false, func(ctx context.Context, c *sql.Conn) error {
			rows, err := c.QueryContext(ctx, "INSERT INTO t VALUES (1) RETURNING x")
			if err == nil {
				rows.Close()
			}
			return err
		}},
		{"BeginTx", "WAL", "BEGIN IMMEDIATE", false, func(ctx context.Context, c *sql.Conn) error {
			_, err := c.ExecContext(ctx, "BEGIN IMMEDIATE")
			if err == nil {
				c.ExecContext(context.Background(), "ROLLBACK")
			}
			return err
		}},
		// Preparing the first statement on a connection reads the schema,
		// which an EXCLUSIVE lock in rollback-journal mode blocks
		{"PrepareContext", "", "BEGIN EXCLUSIVE", true, func(ctx context.Context, c *sql.Conn) error {
			s, err := c.PrepareContext(ctx, "SELECT count(*) FROM t")
			if err == nil {
				s.Close()
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holder, other := busyTestConns(t, dir, "busy-ctx-"+tc.name+".db", "10000", tc.journalMode, tc.lockSQL, tc.otherAfterLock)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := tc.call(ctx, other)
			elapsed := time.Since(start)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("%s returned %v after %v, want %v", tc.name, err, elapsed, context.DeadlineExceeded)
			}
			if elapsed >= busyTimeout/2 {
				t.Errorf("%s waited %v after its context was done, the busy_timeout is %v", tc.name, elapsed, busyTimeout)
			}
			if _, err := holder.ExecContext(context.Background(), "ROLLBACK"); err != nil {
				t.Fatal(err)
			}
			// The connection is usable again
			if _, err := other.ExecContext(context.Background(), "INSERT INTO t VALUES (2)"); err != nil {
				t.Fatalf("the connection after the ended wait: %v", err)
			}
		})
	}
}

// Without a context that ends, busy_timeout keeps its meaning: the wait lasts
// the timeout and fails with SQLITE_BUSY, and a lock released in time is
// taken. PRAGMA busy_timeout still reports the value set.
func TestBusyTimeoutUnchangedWithoutContext(t *testing.T) {
	dir := t.TempDir()
	holder, other := busyTestConns(t, dir, "busy-bg.db", "300", "WAL", "BEGIN IMMEDIATE", false)
	ctx := context.Background()

	// A call with a context that never ends, after which the driver's
	// handler is in place, then one with no deadline at all
	for _, name := range []string{"Background", "LiveContext"} {
		cctx := ctx
		if name == "LiveContext" {
			var cancel context.CancelFunc
			cctx, cancel = context.WithTimeout(ctx, time.Minute)
			defer cancel()
		}
		start := time.Now()
		_, err := other.ExecContext(cctx, "INSERT INTO t VALUES (1)")
		elapsed := time.Since(start)
		var e *Error
		if !errors.As(err, &e) || e.Code() != 5 { // SQLITE_BUSY
			t.Fatalf("%s: got %v after %v, want SQLITE_BUSY", name, err, elapsed)
		}
		if elapsed < 300*time.Millisecond || elapsed > 5*time.Second {
			t.Fatalf("%s: waited %v, want about the 300 ms busy_timeout", name, elapsed)
		}
	}

	var ms int
	if err := other.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&ms); err != nil || ms != 300 {
		t.Fatalf("PRAGMA busy_timeout reports %d, %v; want 300", ms, err)
	}

	// The lock is released while waiting: the write goes through
	go func() {
		time.Sleep(100 * time.Millisecond)
		holder.ExecContext(ctx, "ROLLBACK")
	}()
	if _, err := other.ExecContext(ctx, "INSERT INTO t VALUES (2)"); err != nil {
		t.Fatalf("after the lock was released: %v", err)
	}
}
