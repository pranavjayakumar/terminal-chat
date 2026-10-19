package server

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// #118: an op whose context ends mid-write (session closed, server
// stopping) reports the context error, not sql.ErrTxDone, and isn't logged
// as an ERROR; real failures still are.
func TestWriteCanceledIsContextError(t *testing.T) {
	srv, err := New(Config{DataDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	for range 5 {
		ctx, cancel := context.WithCancel(context.Background())
		_, err := srv.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
			cancel()
			// Wait for database/sql to roll the transaction back on its
			// own, so Commit sees sql.ErrTxDone (the case seen in chaos).
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, err := tx.ExecContext(context.Background(), `SELECT 1`)
				if errors.Is(err, sql.ErrTxDone) {
					return nil, nil
				}
				if time.Now().After(deadline) {
					t.Fatal("transaction never rolled back")
				}
				time.Sleep(time.Millisecond)
			}
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("write after cancel: %v, want context.Canceled", err)
		}
		if opErrLevel(err) != slog.LevelDebug {
			t.Fatalf("%v logged at %v", err, opErrLevel(err))
		}
	}
	if l := opErrLevel(errors.New("disk I/O error")); l != slog.LevelError {
		t.Fatalf("a real failure logged at %v", l)
	}
	if l := opErrLevel(sql.ErrTxDone); l != slog.LevelError {
		t.Fatalf("ErrTxDone without a cancelled context logged at %v", l)
	}
}
