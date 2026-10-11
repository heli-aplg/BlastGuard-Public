package catalog

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"blastguard/internal/db"
)

// FetchedRow is one row read as server-generated SQL literals.
type FetchedRow struct {
	RelName string   // tableoid::regclass (differs from the table for partitions/inheritors)
	CTID    string   //
	Lits    []string // quote_nullable(col) for every column, aligned with TableInfo.Columns
}

// rowSelect builds the SELECT list that turns every column into a literal
// produced by the server, so values round-trip exactly in restore scripts.
func rowSelect(t *TableInfo) string {
	var b strings.Builder
	b.WriteString("SELECT tableoid::regclass::text, ctid::text")
	for _, c := range t.Columns {
		b.WriteString(", quote_nullable(")
		b.WriteString(QuoteIdent(c.Name))
		b.WriteString(")")
	}
	b.WriteString(" FROM ")
	b.WriteString(t.Ref.Q())
	return b.String()
}

func scanLiteralRow(row pgx.CollectableRow) (FetchedRow, error) {
	n := len(row.FieldDescriptions())
	vals := make([]string, n)
	ptrs := make([]any, n)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := row.Scan(ptrs...); err != nil {
		return FetchedRow{}, err
	}
	return FetchedRow{RelName: vals[0], CTID: vals[1], Lits: vals[2:]}, nil
}

// FetchRows reads the rows of t matching pred. pred is built only from
// catalog identifiers and server-quoted literals; user input goes in args.
func FetchRows(ctx context.Context, s *db.Session, t *TableInfo, pred string, args []any) ([]FetchedRow, db.Role, error) {
	return db.Collect(ctx, s, "reading rows of "+t.Ref.Short(), rowSelect(t)+" WHERE "+pred, args, scanLiteralRow)
}

// CountRows counts rows of t matching pred.
func CountRows(ctx context.Context, s *db.Session, t *TableInfo, pred string, args []any) (int64, db.Role, error) {
	out, role, err := db.Collect(ctx, s, "counting rows of "+t.Ref.Short(),
		"SELECT count(*) FROM "+t.Ref.Q()+" WHERE "+pred, args, pgx.RowTo[int64])
	if err != nil {
		return 0, role, err
	}
	return out[0], role, nil
}

// InPredicate renders cols IN (tuples) using literals as-is.
//
//	"a" IN ('1', '2')
//	("a", "b") IN (('1', 'x'), ('2', 'y'))
func InPredicate(cols []string, tuples [][]string) string {
	var b strings.Builder
	if len(cols) == 1 {
		b.WriteString(QuoteIdent(cols[0]))
		b.WriteString(" IN (")
		for i, t := range tuples {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(t[0])
		}
		b.WriteString(")")
		return b.String()
	}
	b.WriteString("(")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(QuoteIdent(c))
	}
	b.WriteString(") IN (")
	for i, t := range tuples {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(" + strings.Join(t, ", ") + ")")
	}
	b.WriteString(")")
	return b.String()
}

// FetchRowsOn reads rows on one specific account (no fallback).
func FetchRowsOn(ctx context.Context, s *db.Session, role db.Role, t *TableInfo, pred string) ([]FetchedRow, error) {
	return db.CollectOn(ctx, s, role, rowSelect(t)+" WHERE "+pred, nil, scanLiteralRow)
}

// CountRowsOn counts rows on one specific account (no fallback).
func CountRowsOn(ctx context.Context, s *db.Session, role db.Role, t *TableInfo, pred string) (int64, error) {
	out, err := db.CollectOn(ctx, s, role, "SELECT count(*) FROM "+t.Ref.Q()+" WHERE "+pred, nil, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	return out[0], nil
}
