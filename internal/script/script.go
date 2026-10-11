// Package script renders delete and restore SQL from an analysis result.
// It never talks to the database: every value is a literal the server produced
// during the analysis.
package script

import (
	"fmt"
	"strings"

	"blastguard/internal/analyzer"
	"blastguard/internal/catalog"
)

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func header(b *strings.Builder, kind string, res *analyzer.Result) {
	fmt.Fprintf(b, "-- blastguard · %s script · generated %s\n", kind, res.GeneratedAt.Format("2006-01-02T15:04:05Z"))
	fmt.Fprintf(b, "-- target : %s WHERE %s = %s\n", res.Target.Ref.Q(), catalog.QuoteIdent(res.PKCol), RootLiteral(res))
	fmt.Fprintf(b, "-- verdict: %s · %s deleted in %s · %s updated\n", res.Verdict,
		plural(res.DeletedRows(), "row"), plural(len(res.Nodes), "table"), plural(len(res.Updates), "row"))
	if n := res.Unverified(); n > 0 {
		fmt.Fprintf(b, "-- WARNING: %d unverified item(s) in the report (triggers / RLS / rules / ...). Read them first.\n", n)
	}
}

// RootLiteral is the server-quoted PK value of the target row.
func RootLiteral(res *analyzer.Result) string {
	if res.Root == nil {
		return "'" + strings.ReplaceAll(res.Input, "'", "''") + "'"
	}
	return res.Root.Lits[res.Target.ColIndex(res.PKCol)]
}

func expect(n int) string { return "  -- expect " + plural(n, "row") }

// Delete renders the delete script. For BLOCKED results it deletes every row
// explicitly, children first; otherwise one DELETE on the target is enough
// because Postgres applies the FK actions itself.
func Delete(res *analyzer.Result) string {
	var b strings.Builder
	header(&b, "DELETE", res)
	b.WriteString("-- Review before running. COMMIT is commented out on purpose: check every\n")
	b.WriteString("-- \"expect\" count, then run COMMIT; otherwise ROLLBACK.\n")
	b.WriteString("BEGIN;\n\n")

	manual := res.Verdict == analyzer.VerdictBlocked
	if manual && res.OrderCycle {
		cteDelete(&b, res)
	} else if manual {
		total := len(res.DeleteOrder)
		for i, oid := range res.DeleteOrder {
			node := res.Nodes[oid]
			fmt.Fprintf(&b, "-- step %d/%d · %s · %s\n", i+1, total, node.Info.Ref.Q(), plural(len(node.Rows), "row"))
			for _, why := range reasons(res, oid) {
				fmt.Fprintf(&b, "--   %s\n", why)
			}
			b.WriteString(deleteStatement(res, node))
			b.WriteString("\n")
		}
	} else {
		fmt.Fprintf(&b, "DELETE FROM %s WHERE %s = %s;%s\n", res.Target.Ref.Q(), catalog.QuoteIdent(res.PKCol), RootLiteral(res), expect(1))
	}
	automatic(&b, res, manual)
	b.WriteString("\n-- COMMIT;\n")
	return b.String()
}

// reasons explains how rows of a table entered the delete set.
func reasons(res *analyzer.Result, oid uint32) []string {
	var out []string
	if oid == res.Target.Ref.OID {
		out = append(out, "target table")
	}
	for _, e := range res.Edges {
		if e.Child.Ref.OID != oid || e.Count == 0 || (e.Kind != analyzer.EdgeCascade && e.Kind != analyzer.EdgeBlock) {
			continue
		}
		verb := "cascades from"
		if e.Blocking() {
			verb = "BLOCKS deletion of"
		}
		out = append(out, fmt.Sprintf("%s %s via %s (ON DELETE %s)", verb, e.Parent.Ref.Short(), e.FK.Name, e.FK.ActionName()))
	}
	return out
}

func deleteStatement(res *analyzer.Result, node *analyzer.Node) string {
	stmt, note := deleteSQL(res, node)
	if note != "" {
		note = "-- " + note + "\n"
	}
	return note + stmt + ";" + expect(len(node.Rows)) + "\n"
}

// deleteSQL renders the DELETE for every row of node (no semicolon) and an
// optional explanatory note.
func deleteSQL(res *analyzer.Result, node *analyzer.Node) (string, string) {
	t := node.Info
	if len(t.PK) > 0 {
		tuples := make([][]string, len(node.Rows))
		for i, r := range node.Rows {
			tuples[i] = analyzer.PKLits(t, r)
		}
		return fmt.Sprintf("DELETE FROM %s WHERE %s", t.Ref.Q(), catalog.InPredicate(t.PK, tuples)), ""
	}
	// No PK: delete by the FK predicates that found the rows.
	var preds []string
	for _, e := range res.Edges {
		if e.Child.Ref.OID == t.Ref.OID && e.Count > 0 && (e.Kind == analyzer.EdgeCascade || e.Kind == analyzer.EdgeBlock) {
			preds = append(preds, "("+e.Pred+")")
		}
	}
	return fmt.Sprintf("DELETE FROM %s WHERE %s", t.Ref.Q(), strings.Join(preds, " OR ")),
		t.Ref.Short() + " has no primary key; rows are selected by the FK predicates that found them"
}

// cteDelete deletes every row in ONE statement. Used when the affected tables
// reference each other in a cycle: with data-modifying CTEs the FK checks run
// once at the end of the statement, so no statement order is needed.
func cteDelete(b *strings.Builder, res *analyzer.Result) {
	b.WriteString("-- The affected tables reference each other in a cycle, so all rows are deleted in\n")
	b.WriteString("-- ONE statement (data-modifying CTEs); FK checks run once, at the end of it.\n")
	b.WriteString("-- The result lists deleted vs expected rows per table: they must be equal.\n")
	var stmts, selects []string
	for i, oid := range res.DeleteOrder {
		node := res.Nodes[oid]
		sql, note := deleteSQL(res, node)
		if note != "" {
			fmt.Fprintf(b, "-- %s\n", note)
		}
		stmts = append(stmts, fmt.Sprintf("  d%d AS (%s RETURNING 1)", i+1, sql))
		selects = append(selects, fmt.Sprintf("SELECT %s AS table_name, (SELECT count(*) FROM d%d) AS deleted, %d AS expected",
			quoteLiteral(node.Info.Ref.Short()), i+1, len(node.Rows)))
	}
	b.WriteString("WITH\n" + strings.Join(stmts, ",\n") + "\n")
	b.WriteString(strings.Join(selects, "\nUNION ALL ") + ";\n")
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// automatic lists what Postgres does on its own after the DELETE(s).
func automatic(b *strings.Builder, res *analyzer.Result, manual bool) {
	var lines []string
	if !manual {
		for _, oid := range res.NodeOrder {
			if oid == res.Target.Ref.OID {
				continue
			}
			n := res.Nodes[oid]
			lines = append(lines, fmt.Sprintf("--   CASCADE   delete %s from %s", plural(len(n.Rows), "row"), n.Info.Ref.Short()))
		}
	}
	type key struct {
		table string
		fk    string
		cols  string
		kind  analyzer.EdgeKind
	}
	counts := map[key]int{}
	var order []key
	for _, u := range res.Updates {
		k := key{u.Table.Ref.Short(), u.Edge.FK.Name, strings.Join(u.Cols, ", "), u.Edge.Kind}
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	for _, k := range order {
		action := "SET NULL "
		if k.kind == analyzer.EdgeSetDefault {
			action = "SET DEFAULT"
		}
		lines = append(lines, fmt.Sprintf("--   %s update %s of %s (%s) via %s", action, plural(counts[k], "row"), k.table, k.cols, k.fk))
	}
	if len(lines) == 0 {
		return
	}
	b.WriteString("\n-- Postgres then applies automatically:\n")
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n")
}

// Restore renders INSERTs for every deleted row (parents first) followed by
// UPDATEs undoing SET NULL / SET DEFAULT.
func Restore(res *analyzer.Result) string {
	var b strings.Builder
	header(&b, "RESTORE", res)
	b.WriteString("-- Re-inserts every deleted row (parents first) and restores columns changed by\n")
	b.WriteString("-- ON DELETE SET NULL / SET DEFAULT. Values are literals read at analysis time.\n")
	for _, w := range restoreTriggerWarnings(res) {
		fmt.Fprintf(&b, "-- WARNING: %s\n", w)
	}
	b.WriteString("BEGIN;\n\n")
	if res.OrderCycle {
		b.WriteString("-- The tables reference each other in a cycle, so all rows are inserted in ONE\n")
		b.WriteString("-- statement (data-modifying CTEs); FK checks run once, at the end of it.\n")
		var stmts, selects []string
		for i := len(res.DeleteOrder) - 1; i >= 0; i-- {
			node := res.Nodes[res.DeleteOrder[i]]
			n := len(stmts) + 1
			stmts = append(stmts, fmt.Sprintf("  i%d AS (\n%s  RETURNING 1)", n, insertSQL(node.Info, node.Rows)))
			selects = append(selects, fmt.Sprintf("SELECT %s AS table_name, (SELECT count(*) FROM i%d) AS inserted, %d AS expected",
				quoteLiteral(node.Info.Ref.Short()), n, len(node.Rows)))
		}
		b.WriteString("WITH\n" + strings.Join(stmts, ",\n") + "\n")
		b.WriteString(strings.Join(selects, "\nUNION ALL ") + ";\n\n")
	} else {
		for i := len(res.DeleteOrder) - 1; i >= 0; i-- {
			node := res.Nodes[res.DeleteOrder[i]]
			fmt.Fprintf(&b, "-- %s · %s\n", node.Info.Ref.Q(), plural(len(node.Rows), "row"))
			b.WriteString(Insert(node.Info, node.Rows))
			b.WriteString("\n")
		}
	}
	if len(res.Updates) > 0 {
		b.WriteString("-- undo ON DELETE SET NULL / SET DEFAULT\n")
		for _, u := range res.Updates {
			b.WriteString(undoUpdate(u))
		}
		b.WriteString("\n")
	}
	b.WriteString("-- COMMIT;\n")
	return b.String()
}

// Insert renders one multi-row INSERT. Generated columns are skipped and
// identity ALWAYS columns get OVERRIDING SYSTEM VALUE.
func Insert(t *catalog.TableInfo, rows []*analyzer.Row) string {
	return strings.TrimSuffix(insertSQL(t, rows), "\n") + ";\n"
}

func insertSQL(t *catalog.TableInfo, rows []*analyzer.Row) string {
	var cols []string
	var idx []int
	override := false
	for i, c := range t.Columns {
		if c.Generated != "" {
			continue
		}
		if c.Identity == "a" {
			override = true
		}
		cols = append(cols, catalog.QuoteIdent(c.Name))
		idx = append(idx, i)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "INSERT INTO %s (%s)", t.Ref.Q(), strings.Join(cols, ", "))
	if override {
		b.WriteString(" OVERRIDING SYSTEM VALUE")
	}
	b.WriteString(" VALUES\n")
	for ri, r := range rows {
		vals := make([]string, len(idx))
		for j, ci := range idx {
			vals[j] = r.Lits[ci]
		}
		b.WriteString("  (" + strings.Join(vals, ", ") + ")")
		if ri < len(rows)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func undoUpdate(u *analyzer.UpdateRow) string {
	t := u.Table
	sets := make([]string, len(u.Cols))
	for i, c := range u.Cols {
		sets[i] = catalog.QuoteIdent(c) + " = " + u.Row.Lits[t.ColIndex(c)]
	}
	if len(t.PK) == 0 {
		var orig []string
		for i, c := range t.Columns {
			orig = append(orig, c.Name+"="+u.Row.Lits[i])
		}
		return fmt.Sprintf("-- %s has no primary key; cannot target the row precisely. Original row: %s\n-- UPDATE %s SET %s WHERE <identify row>;\n",
			t.Ref.Short(), strings.Join(orig, ", "), t.Ref.Q(), strings.Join(sets, ", "))
	}
	return fmt.Sprintf("UPDATE %s SET %s WHERE %s;%s\n", t.Ref.Q(), strings.Join(sets, ", "),
		catalog.InPredicate(t.PK, [][]string{analyzer.PKLits(t, u.Row)}), expect(1))
}

func restoreTriggerWarnings(res *analyzer.Result) []string {
	var out []string
	for _, oid := range res.DeleteOrder {
		t := res.Nodes[oid].Info
		for _, tg := range t.UserTriggers() {
			if tg.OnInsert() && tg.Fires() {
				out = append(out, fmt.Sprintf("trigger %s on %s fires on INSERT and will run again during restore", tg.Name, t.Ref.Short()))
			}
		}
	}
	seen := map[uint32]bool{}
	for _, u := range res.Updates {
		if seen[u.Table.Ref.OID] {
			continue
		}
		seen[u.Table.Ref.OID] = true
		for _, tg := range u.Table.UserTriggers() {
			if tg.OnUpdate() && tg.Fires() {
				out = append(out, fmt.Sprintf("trigger %s on %s fires on UPDATE and will run again during restore", tg.Name, u.Table.Ref.Short()))
			}
		}
	}
	return out
}
