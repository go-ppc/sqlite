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
	"unsafe"

	"modernc.org/libc"
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

	// Releases the lock before the connections close, if the test has not
	t.Cleanup(func() { tx1.Rollback() })
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

		if dc.waitingForLock.Load() {
			return nil, false
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatal("the call neither returned nor waited for the shared-cache lock")
	return nil, false
}

// failWaitNotEnded reports a call that kept waiting after its context was
// done: it releases the lock with release and fails with what the call
// returned then, without waiting longer than ten seconds for it.
func failWaitNotEnded(t *testing.T, what string, release func() error, done <-chan error) {
	t.Helper()

	release()
	select {
	case err := <-done:
		t.Fatalf("%s did not return when its context was done; it returned %v once the lock was released", what, err)
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return when its context was done, nor after the lock was released", what)
	}
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
		// Reading a table another connection is writing waits when the
		// statement runs
		{"QueryContextStep", false, func(ctx context.Context, c *sql.Conn) error {
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
				failWaitNotEnded(t, tc.name, tx1.Rollback, done)
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

// A context done before a wait for a shared-cache lock begins ends the wait
// at once with the context's error, canceled or past its deadline. Here the
// context is set on the connection directly, with no goroutine watching it,
// so nothing but retry can end the wait.
func TestSharedCacheWaitAfterContextDone(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"Canceled", canceled, context.Canceled},
		{"DeadlineExceeded", expired, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, c2, tx1 := sharedCacheConns(t, "sharedcache-early-"+tc.name, false)
			dc2 := driverConn(t, c2)
			s, err := newStmt(dc2, "BEGIN IMMEDIATE")
			if err != nil {
				t.Fatal(err)
			}

			defer s.Close()
			dc2.waitCtx = tc.ctx
			defer func() { dc2.waitCtx = nil }()

			done := make(chan error, 1)
			go func() {
				_, err := dc2.step(s.pstmt)
				done <- err
			}()

			select {
			case err := <-done:
				if !errors.Is(err, tc.want) {
					t.Errorf("the wait ended with %v, want %v", err, tc.want)
				}
			case <-time.After(10 * time.Second):
				failWaitNotEnded(t, "the wait", tx1.Rollback, done)
			}
		})
	}
}

// interruptOnDone and newStmtContext set the context that ends a wait for
// the call they serve and restore the outer call's afterwards, so that a
// later wait is not ended by a context that belonged to a finished call.
func TestSharedCacheWaitContextRestored(t *testing.T) {
	_, _, c2, tx1 := sharedCacheConns(t, "sharedcache-restored", false)
	dc2 := driverConn(t, c2)

	outer := context.WithValue(context.Background(), struct{}{}, "outer")
	inner, innerCancel := context.WithCancel(context.Background())
	dc2.waitCtx = outer
	cleanup := interruptOnDone(inner, dc2, nil)
	if dc2.waitCtx != inner {
		t.Error("interruptOnDone did not record the context of the call")
	}
	cleanup()
	innerCancel()
	if dc2.waitCtx != outer {
		t.Error("interruptOnDone did not restore the outer call's context")
	}
	dc2.waitCtx = nil

	// A statement prepared with a context that is canceled afterwards and
	// then executed without one, so that nothing sets the context of the
	// execution: its wait must not be ended by the preparation's context
	ctx, cancel := context.WithCancel(context.Background())
	stmt, err := c2.PrepareContext(ctx, "INSERT INTO t VALUES(3)")
	if err != nil {
		t.Fatal(err)
	}

	defer stmt.Close()
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := stmt.ExecContext(context.Background())
		done <- err
	}()

	if err, returned := awaitSharedCacheWait(t, dc2, done); returned {
		t.Fatalf("the statement returned without waiting for the lock: %v", err)
	}

	if err := tx1.Commit(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the statement after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the statement did not return after the lock was released")
	}
}

// A wait that the lock's release ends, under a context canceled only
// afterwards, leaves nothing behind that ends or disturbs a later wait.
func TestSharedCacheWaitThenLateCancel(t *testing.T) {
	_, c1, c2, tx1 := sharedCacheConns(t, "sharedcache-late-cancel", false)
	dc2 := driverConn(t, c2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c2.ExecContext(ctx, "BEGIN IMMEDIATE")
		done <- err
	}()

	if err, returned := awaitSharedCacheWait(t, dc2, done); returned {
		t.Fatalf("BEGIN IMMEDIATE returned without waiting for the lock: %v", err)
	}

	if err := tx1.Commit(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("BEGIN IMMEDIATE after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("BEGIN IMMEDIATE did not return after the lock was released")
	}

	cancel()
	if _, err := c2.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}

	tx1b, err := c1.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { tx1b.Rollback() })
	if _, err := tx1b.Exec("INSERT INTO t VALUES(2)"); err != nil {
		t.Fatal(err)
	}

	go func() {
		tx2, err := c2.BeginTx(context.Background(), nil)
		if err == nil {
			err = tx2.Rollback()
		}
		done <- err
	}()

	if err, returned := awaitSharedCacheWait(t, dc2, done); returned {
		t.Fatalf("the second BeginTx returned without waiting for the lock: %v", err)
	}

	if err := tx1b.Commit(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the second BeginTx: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second BeginTx did not return after the lock was released")
	}
}

// Preparing COMMIT or ROLLBACK, like any statement, needs the schema, so
// while another connection is changing it, ending even an empty transaction
// waits for the change, where it failed at once with
// SQLITE_LOCKED_SHAREDCACHE.
func TestSharedCacheTxEndWaitsForSchemaLock(t *testing.T) {
	for _, commit := range []bool{true, false} {
		t.Run(fmt.Sprintf("commit=%v", commit), func(t *testing.T) {
			ctx := context.Background()
			db, err := sql.Open(driverName, fmt.Sprintf("file:sharedcache-txend-%v?mode=memory&cache=shared", commit))
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { db.Close() })
			db.SetMaxOpenConns(2)
			c1, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { c1.Close() })
			c2, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { c2.Close() })
			dc2 := driverConn(t, c2)
			tx2, err := c2.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}

			tx1, err := c1.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { tx1.Rollback() })
			if _, err := tx1.Exec("CREATE TABLE t(x)"); err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() {
				if commit {
					done <- tx2.Commit()
				} else {
					done <- tx2.Rollback()
				}
			}()

			if err, returned := awaitSharedCacheWait(t, dc2, done); returned {
				t.Fatalf("the transaction ended without waiting for the schema lock: %v", err)
			}

			if err := tx1.Commit(); err != nil {
				t.Fatal(err)
			}

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("ending the transaction after the schema lock was released: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the transaction did not end after the schema lock was released")
			}
		})
	}
}

// A context done after the call starts but before it waits for a shared-cache
// lock, here by an authorizer that cancels it while BEGIN is prepared, ends
// the wait at once with the context's error.
func TestSharedCacheBeginTxContextDoneBeforeWait(t *testing.T) {
	_, _, c2, tx1 := sharedCacheConns(t, "sharedcache-done-before-wait", false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setAuthorizer := func(fn AuthorizerFn) {
		t.Helper()
		if err := c2.Raw(func(dc any) error {
			return dc.(AuthorizerRegisterer).RegisterAuthorizer(fn)
		}); err != nil {
			t.Fatal(err)
		}
	}

	setAuthorizer(func(action AuthorizerActionCode, _, _, _, _ string) AuthorizerReturnCode {
		if action == AuthTransaction {
			cancel()
		}
		return AuthorizerOK
	})
	done := make(chan error, 1)
	go func() {
		tx, err := c2.BeginTx(ctx, nil)
		if err == nil {
			tx.Rollback()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("BeginTx returned %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		failWaitNotEnded(t, "BeginTx", tx1.Rollback, done)
	}

	if err := tx1.Rollback(); err != nil {
		t.Fatal(err)
	}

	setAuthorizer(nil)
	if _, err := c2.ExecContext(context.Background(), "BEGIN IMMEDIATE; ROLLBACK"); err != nil {
		t.Fatalf("the connection after the canceled wait: %v", err)
	}
}

// Preparing a statement or beginning a transaction with a context that is
// already done returns the context's error, as executing and querying do,
// rather than preparing, or beginning a transaction that database/sql then
// rolls back.
func TestPrepareAndBeginTxContextDone(t *testing.T) {
	db, err := sql.Open(driverName, ":memory:")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s, err := c.PrepareContext(ctx, "SELECT 1"); !errors.Is(err, context.Canceled) {
		if s != nil {
			s.Close()
		}
		t.Errorf("PrepareContext returned %v, want %v", err, context.Canceled)
	}

	if tx, err := c.BeginTx(ctx, nil); !errors.Is(err, context.Canceled) {
		if tx != nil {
			tx.Rollback()
		}
		t.Errorf("BeginTx returned %v, want %v", err, context.Canceled)
	}
}

// The unlock-notify callback and the context can both try to end a wait, so
// wake unlocks the mutex once, for whichever comes first, and reports which
// one did; a second unlock would be a fatal error.
func TestMutexWakeOnce(t *testing.T) {
	tls := libc.NewTLS()
	defer tls.Close()

	for i := 0; i < 1000; i++ {
		mu := mutexAlloc(tls)
		m := (*mutex)(unsafe.Pointer(mu))
		m.Lock()
		woke := make(chan bool, 2)
		for j := 0; j < 2; j++ {
			go func() { woke <- m.wake() }()
		}

		m.Lock()
		if a, b := <-woke, <-woke; a == b {
			t.Fatalf("wake reported %v and %v, want exactly one true", a, b)
		}

		m.Unlock()
		mutexFree(tls, mu)
	}
}
