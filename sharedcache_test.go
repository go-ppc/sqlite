// Copyright 2026 The Sqlite Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

// sharedCacheConns opens two connections to a fresh shared-cache in-memory
// database. When schemaLock is false, table t is created first, so that a
// write transaction on the first connection holds only a write lock;
// otherwise the first connection's transaction creates t and so also holds
// the schema.
func sharedCacheConns(t *testing.T, name string, schemaLock bool) (db *sql.DB, c1, c2 *sql.Conn, tx1 *sql.Tx) {
	t.Helper()

	ctx := context.Background()
	db, err := sql.Open(driverName, fmt.Sprintf("file:%s?mode=memory&cache=shared&_txlock=immediate", name))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(2)
	if c1, err = db.Conn(ctx); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { c1.Close() })
	if c2, err = db.Conn(ctx); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { c2.Close() })
	if !schemaLock {
		if _, err = c1.ExecContext(ctx, "CREATE TABLE t(x)"); err != nil {
			t.Fatal(err)
		}
	}

	if tx1, err = c1.BeginTx(ctx, nil); err != nil {
		t.Fatal(err)
	}

	stmt := "INSERT INTO t VALUES(1)"
	if schemaLock {
		stmt = "CREATE TABLE t(x)"
	}
	if _, err = tx1.Exec(stmt); err != nil {
		t.Fatal(err)
	}

	return db, c1, c2, tx1
}

// driverConn returns the driver connection behind c.
func driverConn(t *testing.T, c *sql.Conn) (r *conn) {
	t.Helper()

	if err := c.Raw(func(dc any) error {
		r = dc.(*conn)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	return r
}

// awaitSharedCacheWait waits until dc is blocked waiting for a shared-cache
// lock, or until done delivers the result of the call that should have been
// waiting, which is then returned with returned set.
func awaitSharedCacheWait(t *testing.T, dc *conn, done <-chan error) (err error, returned bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			return err, true
		default:
		}

		dc.unlockWaitMu.Lock()
		waiting := dc.unlockWait != 0
		dc.unlockWaitMu.Unlock()
		if waiting {
			return nil, false
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatal("the call neither returned nor waited for the shared-cache lock")
	return nil, false
}

// With a shared cache, a transaction begun while another connection holds
// the lock waits for it, as an executed BEGIN IMMEDIATE does, and starts
// once the lock is released, rather than failing at once with
// SQLITE_LOCKED_SHAREDCACHE. See https://gitlab.com/cznic/sqlite/-/issues/192.
func TestSharedCacheBeginTxWaits(t *testing.T) {
	for _, schemaLock := range []bool{false, true} {
		t.Run(fmt.Sprintf("schemaLock=%v", schemaLock), func(t *testing.T) {
			_, _, c2, tx1 := sharedCacheConns(t, fmt.Sprintf("sharedcache-begin-%v", schemaLock), schemaLock)
			dc2 := driverConn(t, c2)
			done := make(chan error, 1)
			var tx2 *sql.Tx
			go func() {
				var err error
				tx2, err = c2.BeginTx(context.Background(), nil)
				done <- err
			}()

			if err, returned := awaitSharedCacheWait(t, dc2, done); returned {
				tx1.Rollback()
				t.Fatalf("BeginTx returned without waiting for the lock: %v", err)
			}

			if err := tx1.Commit(); err != nil {
				t.Fatal(err)
			}

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("BeginTx after the lock was released: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("BeginTx did not return after the lock was released")
			}

			if _, err := tx2.Exec("INSERT INTO t VALUES(2)"); err != nil {
				t.Fatal(err)
			}

			if err := tx2.Commit(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A call waiting for a shared-cache lock returns the context's error once
// the context is done, while the lock is still held, and leaves the
// connection usable.
func TestSharedCacheWaitEndsWithContext(t *testing.T) {
	for _, tc := range []struct {
		name       string
		schemaLock bool
		call       func(ctx context.Context, c *sql.Conn) error
	}{
		{"BeginTx", false, func(ctx context.Context, c *sql.Conn) error {
			tx, err := c.BeginTx(ctx, nil)
			if err == nil {
				tx.Rollback()
			}
			return err
		}},
		{"ExecContext", false, func(ctx context.Context, c *sql.Conn) error {
			_, err := c.ExecContext(ctx, "BEGIN IMMEDIATE")
			if err == nil {
				c.ExecContext(context.Background(), "ROLLBACK")
			}
			return err
		}},
		// Preparing a query that needs the schema another connection is
		// changing waits before the statement runs
		{"QueryContext", true, func(ctx context.Context, c *sql.Conn) error {
			rows, err := c.QueryContext(ctx, "SELECT x FROM t")
			if err == nil {
				rows.Close()
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, c2, tx1 := sharedCacheConns(t, "sharedcache-ctx-"+tc.name, tc.schemaLock)
			dc2 := driverConn(t, c2)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tc.call(ctx, c2) }()

			if err, returned := awaitSharedCacheWait(t, dc2, done); returned {
				tx1.Rollback()
				t.Fatalf("%s returned without waiting for the lock: %v", tc.name, err)
			}

			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("%s returned %v, want %v", tc.name, err, context.Canceled)
				}
			case <-time.After(10 * time.Second):
				tx1.Rollback()
				t.Fatalf("%s did not return when its context was canceled; it returned %v once the lock was released", tc.name, <-done)
			}

			if err := tx1.Rollback(); err != nil {
				t.Fatal(err)
			}

			// The connection is usable again, with no wait or interrupt left over
			if _, err := c2.ExecContext(context.Background(), "BEGIN IMMEDIATE; ROLLBACK"); err != nil {
				t.Fatalf("the connection after the canceled wait: %v", err)
			}
		})
	}
}

// A context done before the call starts to wait for a shared-cache lock, so
// that interrupt has already run and found no wait to wake, still ends the
// wait at once: interruptOnDone records the call's Done channel on the
// connection, and retry checks it. Here the channel is set directly, with no
// goroutine watching it, so nothing but that check can end the wait.
func TestSharedCacheWaitAfterContextDone(t *testing.T) {
	_, _, c2, tx1 := sharedCacheConns(t, "sharedcache-early", false)
	dc2 := driverConn(t, c2)

	// interruptOnDone sets the channel for the call and restores the outer
	// call's afterwards
	outer, inner := make(chan struct{}), context.Background()
	dc2.callDone = outer
	innerCtx, innerCancel := context.WithCancel(inner)
	cleanup := interruptOnDone(innerCtx, dc2, nil)
	if dc2.callDone != innerCtx.Done() {
		t.Error("interruptOnDone did not record the context's Done channel")
	}
	cleanup()
	innerCancel()
	if dc2.callDone != (<-chan struct{})(outer) {
		t.Error("interruptOnDone did not restore the outer call's Done channel")
	}

	s, err := newStmt(dc2, "BEGIN IMMEDIATE")
	if err != nil {
		t.Fatal(err)
	}

	defer s.Close()
	closed := make(chan struct{})
	close(closed)
	dc2.callDone = closed
	defer func() { dc2.callDone = nil }()

	result := make(chan error, 1)
	go func() {
		_, err := dc2.step(s.pstmt)
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("BEGIN IMMEDIATE succeeded while another connection held the lock")
		}
	case <-time.After(10 * time.Second):
		tx1.Rollback()
		t.Fatalf("the wait did not end for a context done before it began; it returned %v once the lock was released", <-result)
	}

	if err := tx1.Rollback(); err != nil {
		t.Fatal(err)
	}
}
