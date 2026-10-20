// Copyright 2026 The Sqlite Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sqlite

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"

	sqlite3 "modernc.org/sqlite/lib"
)

func TestInterruptBeforeFirstStep(t *testing.T) {
	c, err := newConn(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := newStmt(c, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000) SELECT sum(x) FROM n`)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var done int32
		stop := interruptOnDone(ctx, c, &done)
		defer stop()
		cancel()
		synctest.Wait()
		if atomic.LoadInt32(&done) != 1 {
			t.Fatal("cancellation watcher did not finish")
		}
		if rc, err := c.step(s.pstmt); err == nil || rc != sqlite3.SQLITE_INTERRUPT {
			t.Fatalf("statement executed after cancellation before first step: rc=%d err=%v", rc, err)
		}
	})

	// Cancellation must not remain attached to a subsequent execution.
	if err := c.reset(s.pstmt); err != nil {
		// Reset reports the previous execution's interrupt while resetting the VM.
		if e, ok := err.(*Error); !ok || e.Code() != sqlite3.SQLITE_INTERRUPT {
			t.Fatal(err)
		}
	}
	if rc, err := c.step(s.pstmt); err != nil || rc != sqlite3.SQLITE_ROW {
		t.Fatalf("uncanceled execution: rc=%d err=%v", rc, err)
	}
	if got, err := c.columnInt64(s.pstmt, 0); err != nil || got != 50005000 {
		t.Fatalf("uncanceled result: got=%d err=%v", got, err)
	}

	// Nor to the next call with a context: the flag the progress handler reads
	// outlives the call that set it.
	if err := c.reset(s.pstmt); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := interruptOnDone(ctx, c, nil)
	rc, err := c.step(s.pstmt)
	stop()
	if err != nil || rc != sqlite3.SQLITE_ROW {
		t.Fatalf("execution with a live context after cancellation: rc=%d err=%v", rc, err)
	}
}

// Closing a connection must forget its cancellation flag: the handle value can
// be reused by a connection opened later.
func TestCancellationFlagCloseCleanup(t *testing.T) {
	c, err := newConn(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db := c.db
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interruptOnDone(ctx, c, nil)()
	if _, ok := cancellationFlags.Load(db); !ok {
		t.Fatal("cancellation flag not registered")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := cancellationFlags.Load(db); ok {
		t.Fatal("closed connection retained its cancellation flag")
	}
}
