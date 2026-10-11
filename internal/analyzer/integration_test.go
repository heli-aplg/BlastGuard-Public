package analyzer_test

// Integration tests against a THROWAWAY database. Set BLASTGUARD_TEST_DSN to a
// superuser DSN, e.g. postgres://postgres:postgres@127.0.0.1:55432/postgres.
// The fixture (testdata/fixture.sql) is reloaded first. Generated scripts are
// executed here — only here, inside a transaction that is rolled back — to
// prove that delete + restore returns every table to its original content.

import (
	"context"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"blastguard/internal/analyzer"
	"blastguard/internal/config"
	"blastguard/internal/db"
	"blastguard/internal/report"
	"blastguard/internal/script"
)

type env struct {
	su *pgx.Conn
	ro *db.Account
	bc *db.Account
}

func setup(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("BLASTGUARD_TEST_DSN")
	if dsn == "" {
		t.Skip("BLASTGUARD_TEST_DSN not set")
	}
	ctx := context.Background()
	su, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { su.Close(ctx) })
	fixture, err := os.ReadFile("../../testdata/fixture.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := su.Exec(ctx, string(fixture)); err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	u, _ := url.Parse(dsn)
	port, _ := strconv.Atoi(u.Port())
	dbname := strings.TrimPrefix(u.Path, "/")
	timeouts := config.Timeouts{Connect: 5 * time.Second, Statement: 10 * time.Second, Lock: 2 * time.Second}
	conn := func(user, pw string) *config.Connection {
		return &config.Connection{Host: u.Hostname(), Port: port, Database: dbname, User: user, Password: pw, SSLMode: "disable"}
	}
	ro, err := db.Connect(ctx, db.Readonly, conn("blastguard_ro", "ro"), timeouts)
	if err != nil {
		t.Fatal(err)
	}
	bc, err := db.Connect(ctx, db.Becareful, conn("blastguard_bc", "bc"), timeouts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close(); bc.Close() })
	return &env{su: su, ro: ro, bc: bc}
}

func (e *env) analyze(t *testing.T, bc *db.Account, opt analyzer.Options, table, input string) *analyzer.Result {
	t.Helper()
	res, err := analyzer.Analyze(context.Background(), e.ro, bc, opt, "shop", table, input)
	if err != nil {
		t.Fatalf("analyze %s %s: %v", table, input, err)
	}
	// Rendering must never panic, whatever the verdict.
	_ = report.Render(res, report.Plain{})
	if report.HasScript(res) {
		_ = report.Scripts(res, report.Plain{})
	}
	return res
}

func hasAlert(res *analyzer.Result, level analyzer.AlertLevel, substr string) bool {
	for _, a := range res.Alerts {
		if a.Level == level && strings.Contains(a.Text, substr) {
			return true
		}
	}
	return false
}

func wantVerdict(t *testing.T, res *analyzer.Result, v analyzer.Verdict) {
	t.Helper()
	if res.Verdict != v {
		t.Fatalf("verdict = %s (%s), want %s\n%s", res.Verdict, res.Message, v, report.Render(res, report.Plain{}))
	}
}

// tableDigests fingerprints every table in schema shop except audit_log, which
// the audit trigger legitimately writes to.
func tableDigests(t *testing.T, su *pgx.Conn) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := su.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'shop' AND c.relkind IN ('r', 'p') AND c.relname NOT LIKE 'audit_log%' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, n := range names {
		var d string
		q := `SELECT md5(COALESCE(string_agg(x::text, '|' ORDER BY x::text), '')) FROM ONLY shop.` + n + ` x`
		if err := su.QueryRow(ctx, q).Scan(&d); err != nil {
			t.Fatalf("digest %s: %v", n, err)
		}
		out[n] = d
	}
	return out
}

var reExpect = regexp.MustCompile(`-- expect (\d+) row`)

// roundTrip runs the delete script, checks every "expect" count, runs the
// restore script, compares all tables with the original, then rolls back.
func roundTrip(t *testing.T, e *env, res *analyzer.Result) {
	t.Helper()
	ctx := context.Background()
	before := tableDigests(t, e.su)
	del, rst := script.Delete(res), script.Restore(res)

	results, err := e.su.PgConn().Exec(ctx, del).ReadAll()
	defer func() { _, _ = e.su.PgConn().Exec(ctx, "ROLLBACK").ReadAll() }()
	if err != nil {
		t.Fatalf("delete script failed: %v\n%s", err, del)
	}
	checkCTECounts(t, results, del)
	var got []int64
	for _, r := range results {
		if r.CommandTag.Delete() {
			got = append(got, r.CommandTag.RowsAffected())
		}
	}
	var want []int64
	if !strings.Contains(del, "\nWITH\n") {
		for _, m := range reExpect.FindAllStringSubmatch(del, -1) {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			want = append(want, n)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("DELETE statements = %d, expect comments = %d\n%s", len(got), len(want), del)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("statement %d deleted %d rows, script expected %d\n%s", i+1, got[i], want[i], del)
		}
	}
	rres, err := e.su.PgConn().Exec(ctx, rst).ReadAll()
	if err != nil {
		t.Fatalf("restore script failed: %v\n%s", err, rst)
	}
	checkCTECounts(t, rres, rst)
	after := tableDigests(t, e.su)
	for name, d := range before {
		if after[name] != d {
			t.Errorf("table %s differs after delete+restore\n--- delete\n%s\n--- restore\n%s", name, del, rst)
		}
	}
}

func TestAnalyzer(t *testing.T) {
	e := setup(t)
	opt := analyzer.Options{MaxRows: 1000}

	t.Run("safe independent table", func(t *testing.T) {
		res := e.analyze(t, e.bc, opt, "settings", "site")
		wantVerdict(t, res, analyzer.VerdictSafe)
		if !hasAlert(res, analyzer.LevelInfo, "No user triggers") {
			t.Errorf("missing 'no triggers' info: %+v", res.Alerts)
		}
		if res.Unverified() != 0 {
			t.Errorf("unexpected unverified alerts: %+v", res.Alerts)
		}
		roundTrip(t, e, res)
	})

	t.Run("quoted text key", func(t *testing.T) {
		res := e.analyze(t, e.bc, opt, "settings", "'it''s'")
		wantVerdict(t, res, analyzer.VerdictSafe)
		if got := script.RootLiteral(res); got != "'it''s'" {
			t.Errorf("root literal = %s", got)
		}
		roundTrip(t, e, res)
	})

	t.Run("safe but trigger fires", func(t *testing.T) {
		res := e.analyze(t, e.bc, opt, "customers", "3")
		wantVerdict(t, res, analyzer.VerdictSafe)
		if !hasAlert(res, analyzer.LevelUnverified, "trigger(s) fire") {
			t.Errorf("missing trigger alert: %+v", res.Alerts)
		}
		kinds := map[string]bool{}
		for _, tr := range res.Triggers {
			for _, f := range tr.Findings {
				kinds[f.Kind] = true
			}
		}
		for _, k := range []string{"may-abort", "may-skip", "notify"} {
			if !kinds[k] {
				t.Errorf("missing static finding %q: %+v", k, res.Triggers)
			}
		}
		roundTrip(t, e, res)
	})

	t.Run("cascade with diamond, no-PK child, partitions, RLS, set null, fallback", func(t *testing.T) {
		res := e.analyze(t, e.bc, opt, "customers", "2")
		wantVerdict(t, res, analyzer.VerdictCascade)
		// customer + order 20 + items 200,201 + return 1 + 2 tags + private ref + 2 secrets + 2 activity
		if got := res.DeletedRows(); got != 12 {
			t.Errorf("deleted rows = %d, want 12\n%s", got, report.Render(res, report.Plain{}))
		}
		if len(res.Updates) != 2 {
			t.Errorf("updates = %d, want 2 (notes 2,3)", len(res.Updates))
		}
		if !hasAlert(res, analyzer.LevelDanger, "readonly sees 1") {
			t.Errorf("missing RLS cross-check alert: %+v", res.Alerts)
		}
		fallback := false
		for _, n := range res.Notes {
			if strings.Contains(n, "private_refs") && strings.Contains(n, "becareful") {
				fallback = true
			}
		}
		if !fallback {
			t.Errorf("missing becareful fallback note: %v", res.Notes)
		}
		var langFinding bool
		for _, tr := range res.Triggers {
			for _, f := range tr.Findings {
				if f.Kind == "language" {
					langFinding = true
				}
			}
		}
		if !langFinding {
			t.Error("missing 'language cannot be analyzed' finding for notes_noop")
		}
		roundTrip(t, e, res)
	})

	// Regression: the first becareful use is a permission fallback right after
	// a failed readonly query; the readonly snapshot must stay usable and shared.
	t.Run("permission fallback first keeps snapshot usable", func(t *testing.T) {
		res := e.analyze(t, e.bc, opt, "orders", "20")
		wantVerdict(t, res, analyzer.VerdictCascade)
		if !res.SharedSnapshot {
			t.Errorf("becareful did not share the readonly snapshot: %v", res.Notes)
		}
		roundTrip(t, e, res)
	})

	t.Run("blocked, manual cascade resolves", func(t *testing.T) {
		res := e.analyze(t, e.bc, opt, "customers", "1")
		wantVerdict(t, res, analyzer.VerdictBlocked)
		if len(res.Blockers) != 2 || !res.ManualPossible {
			t.Fatalf("blockers = %d manual = %v\n%s", len(res.Blockers), res.ManualPossible, report.Render(res, report.Plain{}))
		}
		names := map[string]bool{}
		for _, b := range res.Blockers {
			names[b.Edge.FK.Name] = true
		}
		if !names["shipments_item_id_fkey"] || !names["payments_order_id_fkey"] {
			t.Errorf("unexpected blockers: %v", names)
		}
		roundTrip(t, e, res)
	})

	t.Run("blocked, SET NULL into NOT NULL is unresolvable", func(t *testing.T) {
		res := e.analyze(t, e.bc, opt, "customers", "4")
		wantVerdict(t, res, analyzer.VerdictBlocked)
		if res.ManualPossible {
			t.Error("manual script must not be offered")
		}
		if len(report.Options(res)) != 1 {
			t.Errorf("options = %+v", report.Options(res))
		}
	})

	t.Run("set default", func(t *testing.T) {
		res := e.analyze(t, e.bc, opt, "customers", "5")
		wantVerdict(t, res, analyzer.VerdictCascade)
		if !hasAlert(res, analyzer.LevelUnverified, "SET DEFAULT") {
			t.Errorf("missing SET DEFAULT alert: %+v", res.Alerts)
		}
		roundTrip(t, e, res)
	})

	t.Run("self reference cascade", func(t *testing.T) {
		res := e.analyze(t, e.bc, opt, "categories", "1")
		wantVerdict(t, res, analyzer.VerdictCascade)
		if res.DeletedRows() != 3 {
			t.Errorf("deleted = %d, want 3", res.DeletedRows())
		}
		roundTrip(t, e, res)
	})

	for _, tbl := range []string{"employees", "folders"} {
		t.Run("self reference blocked: "+tbl, func(t *testing.T) {
			res := e.analyze(t, e.bc, opt, tbl, "1")
			wantVerdict(t, res, analyzer.VerdictBlocked)
			if !res.ManualPossible {
				t.Fatalf("manual should be possible: %s", res.ManualReason)
			}
			roundTrip(t, e, res)
		})
	}

	t.Run("table cycle uses one CTE statement", func(t *testing.T) {
		res := e.analyze(t, e.bc, opt, "team", "1")
		wantVerdict(t, res, analyzer.VerdictBlocked)
		if !res.OrderCycle || !res.ManualPossible {
			t.Fatalf("cycle=%v manual=%v", res.OrderCycle, res.ManualPossible)
		}
		roundTrip(t, e, res)
	})

	t.Run("composite PK and no PK are unsupported", func(t *testing.T) {
		for _, tbl := range []string{"stock", "order_tags"} {
			res := e.analyze(t, e.bc, opt, tbl, "1")
			wantVerdict(t, res, analyzer.VerdictNoPK)
		}
	})

	t.Run("inheritance duplicate key is a dead end", func(t *testing.T) {
		res := e.analyze(t, e.bc, opt, "events", "1")
		wantVerdict(t, res, analyzer.VerdictDeadEnd)
	})

	t.Run("rule alert", func(t *testing.T) {
		res := e.analyze(t, e.bc, opt, "archived", "1")
		wantVerdict(t, res, analyzer.VerdictSafe)
		if !hasAlert(res, analyzer.LevelUnverified, "rewrites DELETE") {
			t.Errorf("missing rule alert: %+v", res.Alerts)
		}
	})

	t.Run("not found", func(t *testing.T) {
		res := e.analyze(t, e.bc, opt, "settings", "nope")
		wantVerdict(t, res, analyzer.VerdictNotFound)
	})

	t.Run("invalid input", func(t *testing.T) {
		_, err := analyzer.Analyze(context.Background(), e.ro, e.bc, opt, "shop", "customers", "abc")
		if _, ok := err.(*analyzer.InputError); !ok {
			t.Fatalf("err = %v, want InputError", err)
		}
	})

	t.Run("dead end without becareful", func(t *testing.T) {
		res := e.analyze(t, nil, opt, "customers", "2")
		wantVerdict(t, res, analyzer.VerdictDeadEnd)
		if res.DeadEnd == nil || !strings.Contains(res.DeadEnd.Purpose, "private_refs") {
			t.Fatalf("dead end = %+v", res.DeadEnd)
		}
	})

	t.Run("max_rows stops the analysis", func(t *testing.T) {
		res := e.analyze(t, e.bc, analyzer.Options{MaxRows: 3}, "customers", "2")
		wantVerdict(t, res, analyzer.VerdictDeadEnd)
		if !strings.Contains(res.Message, "max_rows") {
			t.Errorf("message = %s", res.Message)
		}
	})
}

func TestSessionIsReadOnly(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	s, err := db.Begin(ctx, e.ro, e.bc)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	if _, _, err := db.Collect(ctx, s, "x", "DELETE FROM shop.settings", nil, pgx.RowTo[int64]); err == nil {
		t.Fatal("guard accepted a DELETE")
	}
	// Even a write hidden in a CTE is rejected by the read-only transaction.
	_, _, err = db.Collect(ctx, s, "x", "WITH d AS (DELETE FROM shop.settings RETURNING 1) SELECT count(*) FROM d", nil, pgx.RowTo[int64])
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected read-only transaction error, got %v", err)
	}
	var n int
	if err := e.su.QueryRow(ctx, "SELECT count(*) FROM shop.settings").Scan(&n); err != nil || n != 2 {
		t.Fatalf("settings rows = %d (%v)", n, err)
	}
}

// checkCTECounts verifies the "table_name | done | expected" rows printed by
// single-statement (CTE) scripts.
func checkCTECounts(t *testing.T, results []*pgconn.Result, sql string) {
	t.Helper()
	for _, r := range results {
		if len(r.FieldDescriptions) != 3 || string(r.FieldDescriptions[0].Name) != "table_name" {
			continue
		}
		if len(r.Rows) == 0 {
			t.Fatalf("CTE script returned no count rows\n%s", sql)
		}
		for _, row := range r.Rows {
			if string(row[1]) != string(row[2]) {
				t.Fatalf("%s: done %s rows, expected %s\n%s", row[0], row[1], row[2], sql)
			}
		}
	}
}
