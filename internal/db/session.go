package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// readOnlyTx is the only transaction mode this tool ever uses (layer L2).
var readOnlyTx = pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}

// Session is one consistent, read-only view of the database used for a single
// analysis. Queries run on readonly first and fall back to becareful on
// permission / timeout errors. Close always rolls back.
type Session struct {
	mu   sync.Mutex
	ro   *Account
	bc   *Account
	roTx pgx.Tx
	bcTx pgx.Tx

	// snapshot is the readonly snapshot id exported at Begin ("" if the
	// server refused, e.g. on a hot standby).
	snapshot string
	// SharedSnapshot is true when becareful imported the readonly snapshot.
	SharedSnapshot bool
	bcOpened       bool
	bcOpenErr      error
	Notes          []string
}

// Begin opens the readonly transaction. bc may be nil.
func Begin(ctx context.Context, ro, bc *Account) (*Session, error) {
	if !ro.Connected() {
		return nil, errors.New("readonly connection is not available")
	}
	tx, err := ro.conn.BeginTx(ctx, readOnlyTx)
	if err != nil {
		return nil, err
	}
	s := &Session{ro: ro, bc: bc, roTx: tx}
	// Export the snapshot as the first statement: Postgres refuses to export
	// from a subtransaction, so it cannot be wrapped in a savepoint. If the
	// server refuses (e.g. some standbys), start over without sharing.
	if bc != nil {
		if err := tx.QueryRow(ctx, `SELECT pg_catalog.pg_export_snapshot()`).Scan(&s.snapshot); err != nil {
			s.snapshot = ""
			_ = tx.Rollback(ctx)
			if s.roTx, err = ro.conn.BeginTx(ctx, readOnlyTx); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

// Close rolls back every open transaction. It never commits.
func (s *Session) Close(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.roTx != nil {
		_ = s.roTx.Rollback(ctx)
		s.roTx = nil
	}
	if s.bcTx != nil {
		_ = s.bcTx.Rollback(ctx)
		s.bcTx = nil
	}
}

// HasBecareful reports whether a becareful account is configured and connected.
func (s *Session) HasBecareful() bool { return s.bc.Connected() }

// DeadEndError means no account could run a required query (spec P3).
type DeadEndError struct {
	Purpose string
	SQL     string
	Errs    map[Role]error
}

func (e *DeadEndError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "dead end while %s:", e.Purpose)
	for _, r := range []Role{Readonly, Becareful} {
		if err, ok := e.Errs[r]; ok {
			fmt.Fprintf(&b, " [%s] %v;", r, err)
		}
	}
	return b.String()
}

// Fallbackable reports whether err should be retried on the becareful account:
// missing privilege, statement timeout, or lock timeout.
func Fallbackable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42501", "57014", "55P03":
			return true
		}
	}
	return false
}

// ErrNoBecareful is reported as the becareful error when it is not connected.
var ErrNoBecareful = errors.New("becareful account not available")

// Collect runs sql (read-only, inside a savepoint) and collects rows with fn.
// It returns which role produced the result.
func Collect[T any](ctx context.Context, s *Session, purpose, sql string, args []any, fn pgx.RowToFunc[T]) ([]T, Role, error) {
	if err := guard(sql); err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out, err := collectOn(ctx, s.roTx, sql, args, fn)
	if err == nil {
		return out, Readonly, nil
	}
	if !Fallbackable(err) {
		return nil, Readonly, fmt.Errorf("%s: %w", purpose, err)
	}
	roErr := err
	bcTx, err := s.becarefulTx(ctx)
	if err != nil {
		return nil, "", &DeadEndError{Purpose: purpose, SQL: sql, Errs: map[Role]error{Readonly: roErr, Becareful: err}}
	}
	out, err = collectOn(ctx, bcTx, sql, args, fn)
	if err != nil {
		return nil, "", &DeadEndError{Purpose: purpose, SQL: sql, Errs: map[Role]error{Readonly: roErr, Becareful: err}}
	}
	s.Notes = append(s.Notes, fmt.Sprintf("%s: readonly failed (%s), used becareful", purpose, ShortErr(roErr)))
	return out, Becareful, nil
}

// CollectOn runs sql on one specific role without fallback (used for
// cross-checks such as trigger visibility).
func CollectOn[T any](ctx context.Context, s *Session, role Role, sql string, args []any, fn pgx.RowToFunc[T]) ([]T, error) {
	if err := guard(sql); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if role == Readonly {
		return collectOn(ctx, s.roTx, sql, args, fn)
	}
	tx, err := s.becarefulTx(ctx)
	if err != nil {
		return nil, err
	}
	return collectOn(ctx, tx, sql, args, fn)
}

// collectOn wraps the query in a savepoint so a failed query does not abort
// the surrounding snapshot transaction. The savepoint is always released, so
// the transaction never stays inside a subtransaction between queries.
func collectOn[T any](ctx context.Context, tx pgx.Tx, sql string, args []any, fn pgx.RowToFunc[T]) ([]T, error) {
	if _, err := tx.Exec(ctx, "SAVEPOINT blastguard_q"); err != nil {
		return nil, err
	}
	out, err := collectRows(ctx, tx, sql, args, fn)
	if err != nil {
		if _, rbErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT blastguard_q"); rbErr != nil {
			return nil, errors.Join(err, rbErr)
		}
	}
	// Releasing a savepoint is not a write; the outer transaction is still
	// read-only and is rolled back in Close.
	if _, relErr := tx.Exec(ctx, "RELEASE SAVEPOINT blastguard_q"); relErr != nil && err == nil {
		err = relErr
	}
	return out, err
}

func collectRows[T any](ctx context.Context, tx pgx.Tx, sql string, args []any, fn pgx.RowToFunc[T]) ([]T, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, fn)
}

// becarefulTx lazily opens the becareful transaction, importing the readonly
// snapshot when the server allows it. Caller holds s.mu.
func (s *Session) becarefulTx(ctx context.Context) (pgx.Tx, error) {
	if s.bcOpened {
		return s.bcTx, s.bcOpenErr
	}
	s.bcOpened = true
	if !s.HasBecareful() {
		s.bcOpenErr = ErrNoBecareful
		return nil, s.bcOpenErr
	}
	if s.snapshot != "" {
		tx, err := s.bc.conn.BeginTx(ctx, readOnlyTx)
		if err == nil {
			if _, err = tx.Exec(ctx, `SET TRANSACTION SNAPSHOT '`+strings.ReplaceAll(s.snapshot, `'`, `''`)+`'`); err == nil {
				s.bcTx, s.SharedSnapshot = tx, true
				return tx, nil
			}
			_ = tx.Rollback(ctx)
		}
	}
	tx, err := s.bc.conn.BeginTx(ctx, readOnlyTx)
	if err != nil {
		s.bcOpenErr = err
		return nil, err
	}
	s.bcTx = tx
	s.Notes = append(s.Notes, "becareful could not share the readonly snapshot; its reads use a separate snapshot")
	return tx, nil
}

// guard is layer L4: only plain read statements are accepted.
func guard(sql string) error {
	head := strings.ToUpper(firstWord(sql))
	switch head {
	case "SELECT", "WITH", "SHOW":
		return nil
	}
	return fmt.Errorf("blocked non-read statement (starts with %q)", head)
}

func firstWord(sql string) string {
	s := strings.TrimSpace(sql)
	for strings.HasPrefix(s, "--") || strings.HasPrefix(s, "/*") {
		if strings.HasPrefix(s, "--") {
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = strings.TrimSpace(s[i+1:])
			} else {
				return ""
			}
		} else {
			if i := strings.Index(s, "*/"); i >= 0 {
				s = strings.TrimSpace(s[i+2:])
			} else {
				return ""
			}
		}
	}
	end := strings.IndexFunc(s, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') })
	if end < 0 {
		return s
	}
	return s[:end]
}

func ShortErr(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code + " " + pgErr.Message
	}
	return err.Error()
}

// ServerVersionNum is the readonly server's version (e.g. 160015).
func (s *Session) ServerVersionNum() int { return s.ro.VersionNum }
