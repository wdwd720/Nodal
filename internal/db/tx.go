package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SerializableMaxRetries is the retry budget used by Serializable.
const SerializableMaxRetries = 5

// TxOptions controls InTx. Isolation "" means the server default (READ
// COMMITTED). MaxRetries is the number of additional attempts after the first
// when the transaction fails with SQLSTATE 40001/40P01.
type TxOptions struct {
	Isolation  pgx.TxIsoLevel
	ReadOnly   bool
	MaxRetries int
}

// InTx runs fn inside a transaction. fn's error rolls back and is returned
// unchanged (so errors.Is/As keep working). A panic inside fn rolls back and
// is re-raised. Serialization failures and deadlocks (from fn or from COMMIT)
// re-run the whole transaction up to opts.MaxRetries times with exponential
// backoff and jitter; nothing else is ever retried, and side effects outside
// the transaction are the caller's responsibility to make idempotent.
func (db *DB) InTx(ctx context.Context, opts TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return runWithRetry(ctx, opts.MaxRetries, sleepWithContext, func(ctx context.Context) error {
		return db.runTx(ctx, opts, fn)
	})
}

// Serializable runs fn under SERIALIZABLE isolation with SerializableMaxRetries.
// Use it for every financial state change (PART 22).
func (db *DB) Serializable(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return db.InTx(ctx, TxOptions{Isolation: pgx.Serializable, MaxRetries: SerializableMaxRetries}, fn)
}

func (db *DB) runTx(ctx context.Context, opts TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) error {
	txo := pgx.TxOptions{IsoLevel: opts.Isolation}
	if opts.ReadOnly {
		txo.AccessMode = pgx.ReadOnly
	}
	tx, err := db.pool.BeginTx(ctx, txo)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}

	committed := false
	defer func() {
		if p := recover(); p != nil {
			rollback(ctx, tx)
			panic(p)
		}
		if !committed {
			rollback(ctx, tx)
		}
	}()

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	committed = true
	return nil
}

// rollback best-effort rolls back on a context that survives cancellation of
// the caller's ctx so the connection is returned clean when possible. pgx
// discards the connection if rollback fails, so the error is safe to drop.
func rollback(ctx context.Context, tx pgx.Tx) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := tx.Rollback(rctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		_ = err // connection is discarded by pgx; nothing further to do
	}
}
