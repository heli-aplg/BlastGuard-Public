package analyzer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"blastguard/internal/catalog"
	"blastguard/internal/db"
)

type Options struct {
	MaxRows   int
	AIEnabled bool
	// Analyzers run on every relevant trigger. Defaults to the static analyzer.
	Analyzers []TriggerAnalyzer
}

// InputError is a problem with what the user typed (not with the database).
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

// NormalizeInput trims the pasted Row ID and strips one pair of surrounding
// single quotes. Only one row is accepted.
func NormalizeInput(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	if s == "" {
		return "", &InputError{"Row ID is empty"}
	}
	if strings.ContainsAny(s, "\r\n") {
		return "", &InputError{"only a single Row ID is supported (input contains a line break)"}
	}
	return s, nil
}

type batch struct {
	table uint32
	rows  []*Row
	edge  int // edge that produced this batch, -1 for the target
	depth int
	auto  bool
}

type analysis struct {
	ctx    context.Context
	s      *db.Session
	opt    Options
	res    *Result
	tables map[uint32]*catalog.TableInfo
	total  int64
	queue  []batch
}

// Analyze evaluates deleting schema.table WHERE <pk> = input. It never writes:
// every query runs inside a read-only snapshot that is rolled back at the end.
func Analyze(ctx context.Context, ro, bc *db.Account, opt Options, schema, table, rawInput string) (*Result, error) {
	input, err := NormalizeInput(rawInput)
	if err != nil {
		return nil, err
	}
	if opt.MaxRows <= 0 {
		opt.MaxRows = 10000
	}
	if len(opt.Analyzers) == 0 {
		opt.Analyzers = []TriggerAnalyzer{StaticAnalyzer{}}
	}
	s, err := db.Begin(ctx, ro, bc)
	if err != nil {
		return nil, err
	}
	defer s.Close(context.Background())

	a := &analysis{
		ctx: ctx, s: s, opt: opt,
		res:    &Result{GeneratedAt: time.Now().UTC(), Input: input, Nodes: map[uint32]*Node{}},
		tables: map[uint32]*catalog.TableInfo{},
	}
	err = a.run(schema, table)
	a.res.Notes = append(a.res.Notes, s.Notes...)
	a.res.SharedSnapshot = s.SharedSnapshot
	var dead *db.DeadEndError
	switch {
	case err == nil:
		return a.res, nil
	case errors.As(err, &dead):
		a.res.Verdict = VerdictDeadEnd
		a.res.DeadEnd = dead
		a.res.Message = "Analysis stopped: no account could " + dead.Purpose + "."
		return a.res, nil
	default:
		return nil, err
	}
}

func (a *analysis) table(oid uint32) (*catalog.TableInfo, error) {
	if t, ok := a.tables[oid]; ok {
		return t, nil
	}
	t, err := catalog.LoadTableByOID(a.ctx, a.s, oid)
	if err != nil {
		return nil, err
	}
	a.tables[oid] = t
	return t, nil
}

func (a *analysis) run(schema, table string) error {
	res := a.res
	info, err := catalog.LoadTable(a.ctx, a.s, schema, table)
	if err != nil {
		return err
	}
	a.tables[info.Ref.OID] = info
	res.Target = info

	if len(info.PK) != 1 {
		res.Verdict = VerdictNoPK
		if len(info.PK) == 0 {
			res.Message = "Table has no primary key; Row ID input is not supported."
		} else {
			res.Message = fmt.Sprintf("Table has a composite primary key (%s); only single-column keys are supported.", strings.Join(info.PK, ", "))
		}
		return nil
	}
	res.PKCol = info.PK[0]
	pk := info.Column(res.PKCol)
	pred := catalog.QuoteIdent(pk.Name) + " = CAST($1::text AS " + pk.BaseType + ")"
	rows, role, err := catalog.FetchRows(a.ctx, a.s, info, pred, []any{res.Input})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "22") {
			return &InputError{fmt.Sprintf("Row ID %q is not a valid %s: %s", res.Input, pk.BaseType, pgErr.Message)}
		}
		return err
	}
	res.RootSource = role
	switch {
	case len(rows) == 0:
		res.Verdict = VerdictNotFound
		res.Message = fmt.Sprintf("No row in %s with %s = %s.", info.Ref.Short(), pk.Name, quoteForMsg(res.Input))
		if info.RowSecurity {
			res.alert(LevelUnverified, fmt.Sprintf("Row-level security is enabled on %s; the row may exist but be hidden from the %s account.", info.Ref.Short(), role))
		}
		return nil
	case len(rows) > 1:
		res.Verdict = VerdictDeadEnd
		res.Message = fmt.Sprintf("%s = %s matched %d rows (inheritance children share the key); a single-row analysis is not possible.",
			pk.Name, quoteForMsg(res.Input), len(rows))
		return nil
	}

	root := &Row{Key: rowKey(info, rows[0]), Lits: rows[0].Lits, Depth: 0, Auto: true}
	res.Root = root
	a.addDelete(info, []*Row{root}, true)
	a.total = 1
	a.queue = []batch{{table: info.Ref.OID, rows: []*Row{root}, edge: -1, depth: 0, auto: true}}

	if err := a.traverse(); err != nil {
		return err
	}
	if res.Verdict == VerdictDeadEnd {
		return nil
	}
	a.finishUpdates()
	a.findBlockers()
	a.structuralAlerts()
	if err := a.analyzeTriggers(); err != nil {
		return err
	}
	a.planOrder()
	a.decide()
	return nil
}

func quoteForMsg(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func rowKey(t *catalog.TableInfo, fr catalog.FetchedRow) string {
	if len(t.PK) == 0 {
		return fr.RelName + ":" + fr.CTID
	}
	parts := make([]string, len(t.PK))
	for i, c := range t.PK {
		parts[i] = fr.Lits[t.ColIndex(c)]
	}
	return strings.Join(parts, "\x1f")
}

// PKLits returns the PK literals of a row (nil when the table has no PK).
func PKLits(t *catalog.TableInfo, r *Row) []string {
	if len(t.PK) == 0 {
		return nil
	}
	out := make([]string, len(t.PK))
	for i, c := range t.PK {
		out[i] = r.Lits[t.ColIndex(c)]
	}
	return out
}

func (a *analysis) node(t *catalog.TableInfo) *Node {
	n, ok := a.res.Nodes[t.Ref.OID]
	if !ok {
		n = &Node{Info: t, byKey: map[string]*Row{}}
		a.res.Nodes[t.Ref.OID] = n
		a.res.NodeOrder = append(a.res.NodeOrder, t.Ref.OID)
	}
	return n
}

// addDelete puts rows in the delete set and returns the ones not seen before.
// A row reached again through an all-CASCADE path is upgraded to Auto.
func (a *analysis) addDelete(t *catalog.TableInfo, rows []*Row, auto bool) []*Row {
	n := a.node(t)
	var fresh []*Row
	for _, r := range rows {
		if known, ok := n.byKey[r.Key]; ok {
			if auto {
				known.Auto = true
			}
			continue
		}
		r.Auto = auto
		n.byKey[r.Key] = r
		n.Rows = append(n.Rows, r)
		fresh = append(fresh, r)
	}
	return fresh
}

// parentTuples collects distinct non-NULL values of cols across rows (MATCH
// SIMPLE: a NULL in any column means the FK does not apply).
func parentTuples(t *catalog.TableInfo, rows []*Row, cols []string) [][]string {
	idx := make([]int, len(cols))
	for i, c := range cols {
		idx[i] = t.ColIndex(c)
	}
	seen := map[string]bool{}
	var out [][]string
	for _, r := range rows {
		tuple := make([]string, len(idx))
		hasNull := false
		for i, ci := range idx {
			if ci < 0 || r.Lits[ci] == "NULL" {
				hasNull = true
				break
			}
			tuple[i] = r.Lits[ci]
		}
		if hasNull {
			continue
		}
		k := strings.Join(tuple, "\x1f")
		if !seen[k] {
			seen[k] = true
			out = append(out, tuple)
		}
	}
	return out
}

func edgeKind(action string) EdgeKind {
	switch action {
	case "c":
		return EdgeCascade
	case "n":
		return EdgeSetNull
	case "d":
		return EdgeSetDefault
	default:
		return EdgeBlock
	}
}

func (a *analysis) traverse() error {
	res := a.res
	for qi := 0; qi < len(a.queue); qi++ {
		b := a.queue[qi]
		parent := a.tables[b.table]
		for _, fk := range parent.Incoming {
			tuples := parentTuples(parent, b.rows, fk.ParentCols)
			if len(tuples) == 0 {
				continue
			}
			child, err := a.table(fk.ChildOID)
			if err != nil {
				return err
			}
			e := &Edge{
				ID: len(res.Edges), ParentEdge: b.edge, Parent: parent, Child: child, FK: fk,
				Kind: edgeKind(fk.DelAction), Depth: b.depth + 1,
				Pred: catalog.InPredicate(fk.ChildCols, tuples),
			}
			res.Edges = append(res.Edges, e)
			cnt, role, err := catalog.CountRows(a.ctx, a.s, child, e.Pred, nil)
			if err != nil {
				return err
			}
			e.Count, e.Source = cnt, role
			if child.RowSecurity && role == db.Readonly && a.s.HasBecareful() {
				// RLS filters silently: compare with becareful and keep the larger view.
				if bcCnt, err := catalog.CountRowsOn(a.ctx, a.s, db.Becareful, child, e.Pred); err == nil && bcCnt != cnt {
					res.alert(LevelDanger, fmt.Sprintf("Row-level security on %s: readonly sees %d referencing row(s) via %s, becareful sees %d; using becareful data.",
						child.Ref.Short(), cnt, fk.Name, bcCnt))
					e.Count, e.Source, cnt = bcCnt, db.Becareful, bcCnt
				}
			}
			if cnt == 0 {
				continue
			}
			a.total += cnt
			if a.total > int64(a.opt.MaxRows) {
				res.Verdict = VerdictDeadEnd
				res.Message = fmt.Sprintf("Impact exceeds max_rows (%d) at %s via %s; analysis stopped, nothing below this point was verified.",
					a.opt.MaxRows, child.Ref.Short(), fk.Name)
				return nil
			}
			fetched, err := a.fetchChildRows(child, e)
			if err != nil {
				return err
			}
			rows := make([]*Row, len(fetched))
			for i, fr := range fetched {
				rows[i] = &Row{Key: rowKey(child, fr), Lits: fr.Lits, Depth: e.Depth}
			}
			switch e.Kind {
			case EdgeCascade, EdgeBlock:
				auto := b.auto && e.Kind == EdgeCascade
				fresh := a.addDelete(child, rows, auto)
				e.NewRows = len(fresh)
				n := res.Nodes[child.Ref.OID]
				for _, r := range rows {
					e.Rows = append(e.Rows, n.byKey[r.Key])
				}
				if len(fresh) > 0 {
					a.queue = append(a.queue, batch{table: child.Ref.OID, rows: fresh, edge: e.ID, depth: e.Depth, auto: auto})
				}
			case EdgeSetNull, EdgeSetDefault:
				e.Rows = rows
				e.NewRows = len(rows)
				for _, r := range rows {
					res.Updates = append(res.Updates, &UpdateRow{Table: child, Row: r, Edge: e, Cols: fk.AffectedCols()})
				}
			}
		}
	}
	return nil
}

// fetchChildRows reads the rows matched by e on the account that counted them.
func (a *analysis) fetchChildRows(child *catalog.TableInfo, e *Edge) ([]catalog.FetchedRow, error) {
	if e.Source == db.Becareful && child.RowSecurity {
		return catalog.FetchRowsOn(a.ctx, a.s, db.Becareful, child, e.Pred)
	}
	rows, _, err := catalog.FetchRows(a.ctx, a.s, child, e.Pred, nil)
	return rows, err
}

// finishUpdates drops SET NULL/DEFAULT rows that are deleted anyway and
// merges multiple FKs hitting the same row.
func (a *analysis) finishUpdates() {
	var kept []*UpdateRow
	for _, u := range a.res.Updates {
		if n, ok := a.res.Nodes[u.Table.Ref.OID]; ok {
			if _, deleted := n.byKey[u.Row.Key]; deleted {
				continue
			}
		}
		kept = append(kept, u)
	}
	a.res.Updates = kept
}

func (a *analysis) isDeleted(t *catalog.TableInfo, key string) bool {
	n, ok := a.res.Nodes[t.Ref.OID]
	if !ok {
		return false
	}
	_, ok = n.byKey[key]
	return ok
}

func (a *analysis) findBlockers() {
	res := a.res
	for _, e := range res.Edges {
		if e.Count == 0 {
			continue
		}
		switch e.Kind {
		case EdgeBlock:
			var orderDependent bool
			for _, r := range e.Rows {
				if r.Auto && e.FK.DelAction == "a" {
					continue // removed by CASCADE in the same statement; NO ACTION checks at statement end
				}
				if r.Auto {
					orderDependent = true
				}
				e.Unresolved = append(e.Unresolved, r)
			}
			if len(e.Unresolved) == 0 {
				res.Notes = append(res.Notes, fmt.Sprintf("%s (%s): %d referencing row(s) are removed by CASCADE in the same statement, so NO ACTION does not block.",
					e.FK.Name, e.Child.Ref.Short(), len(e.Rows)))
				continue
			}
			reason := fmt.Sprintf("ON DELETE %s and %d row(s) still reference the deleted row(s)", e.FK.ActionName(), len(e.Unresolved))
			if orderDependent {
				reason += "; some are also removed by CASCADE, but RESTRICT is checked per row, so the outcome depends on trigger order (UNVERIFIED)"
			}
			res.Blockers = append(res.Blockers, Blocker{Edge: e, Rows: e.Unresolved, Reason: reason, Resolvable: true, Path: res.PathTo(e)})
		case EdgeSetNull:
			var notNull []string
			for _, col := range e.FK.AffectedCols() {
				if c := e.Child.Column(col); c != nil && c.NotNull {
					notNull = append(notNull, col)
				}
			}
			if len(notNull) == 0 {
				continue
			}
			var stuck []*Row
			for _, r := range e.Rows {
				if !a.isDeleted(e.Child, r.Key) {
					stuck = append(stuck, r)
				}
			}
			if len(stuck) > 0 {
				res.Blockers = append(res.Blockers, Blocker{
					Edge: e, Rows: stuck, Resolvable: false, Path: res.PathTo(e),
					Reason: fmt.Sprintf("ON DELETE SET NULL targets NOT NULL column(s) %s; Postgres will raise not_null_violation", strings.Join(notNull, ", ")),
				})
			}
		}
	}
}

// impacted returns every table in the delete set or the update set.
func (a *analysis) impacted() (deleted, updated []*catalog.TableInfo) {
	for _, oid := range a.res.NodeOrder {
		deleted = append(deleted, a.res.Nodes[oid].Info)
	}
	seen := map[uint32]bool{}
	for _, u := range a.res.Updates {
		if !seen[u.Table.Ref.OID] {
			seen[u.Table.Ref.OID] = true
			updated = append(updated, u.Table)
		}
	}
	return deleted, updated
}

func (a *analysis) structuralAlerts() {
	res := a.res
	deleted, updated := a.impacted()
	all := append(append([]*catalog.TableInfo{}, deleted...), updated...)
	seen := map[uint32]bool{}
	for _, t := range all {
		if seen[t.Ref.OID] {
			continue
		}
		seen[t.Ref.OID] = true
		if t.RowSecurity {
			res.alert(LevelUnverified, fmt.Sprintf("Row-level security is enabled on %s: rows hidden from the reading account are not counted, while FK enforcement still sees them.", t.Ref.Short()))
		}
	}
	for _, t := range deleted {
		if t.HasInheritors {
			res.alert(LevelUnverified, fmt.Sprintf("%s has inheritance children; DELETE without ONLY also affects them.", t.Ref.Short()))
		}
		for _, r := range t.Rules {
			if r.Event == "4" {
				res.alert(LevelUnverified, fmt.Sprintf("Rule %q on %s rewrites DELETE: %s", r.Name, t.Ref.Short(), oneLine(r.Def)))
			}
		}
	}
	for _, t := range updated {
		for _, r := range t.Rules {
			if r.Event == "2" {
				res.alert(LevelUnverified, fmt.Sprintf("Rule %q on %s rewrites UPDATE (fired by SET NULL/DEFAULT): %s", r.Name, t.Ref.Short(), oneLine(r.Def)))
			}
		}
	}
	for _, e := range res.Edges {
		if e.Count == 0 {
			continue
		}
		for _, tg := range e.Parent.Triggers {
			if tg.Internal && tg.ConstraintOID == e.FK.OID && !tg.Fires() {
				res.alert(LevelDanger, fmt.Sprintf("FK %s enforcement trigger %s on %s is %s: Postgres will not apply ON DELETE %s, so this analysis does not match runtime behaviour.",
					e.FK.Name, tg.Name, e.Parent.Ref.Short(), tg.EnabledText(), e.FK.ActionName()))
			}
		}
		if e.Kind == EdgeSetDefault {
			res.alert(LevelUnverified, fmt.Sprintf("%s uses ON DELETE SET DEFAULT on %s(%s): the default value must exist in %s or the DELETE fails; defaults are not evaluated by this tool.",
				e.FK.Name, e.Child.Ref.Short(), strings.Join(e.FK.AffectedCols(), ", "), e.Parent.Ref.Short()))
		}
		if e.Kind == EdgeSetNull || e.Kind == EdgeSetDefault {
			for _, in := range e.Child.Incoming {
				if overlaps(in.ParentCols, e.FK.AffectedCols()) {
					res.alert(LevelUnverified, fmt.Sprintf("Columns changed by %s are referenced by FK %s from %s; that FK's ON UPDATE action also fires.",
						e.FK.Name, in.Name, in.Child().Short()))
				}
			}
		}
	}
	if !a.s.SharedSnapshot && a.s.HasBecareful() {
		for _, n := range a.s.Notes {
			if strings.Contains(n, "separate snapshot") {
				res.alert(LevelUnverified, "Some reads came from becareful on a separate snapshot; data may differ slightly between accounts.")
				break
			}
		}
	}
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// planOrder computes a children-first table order for a manual script.
func (a *analysis) planOrder() {
	res := a.res
	tables := res.NodeOrder
	in := map[uint32]bool{}
	for _, t := range tables {
		in[t] = true
	}
	children := map[uint32]map[uint32]bool{}
	indeg := map[uint32]int{}
	for _, e := range res.Edges {
		p, c := e.Parent.Ref.OID, e.Child.Ref.OID
		if e.Count == 0 || p == c || !in[p] || !in[c] || (e.Kind != EdgeCascade && e.Kind != EdgeBlock) {
			continue
		}
		if children[p] == nil {
			children[p] = map[uint32]bool{}
		}
		if !children[p][c] {
			children[p][c] = true
			indeg[c]++
		}
	}
	var order []uint32 // parents first
	var ready []uint32
	for _, t := range tables {
		if indeg[t] == 0 {
			ready = append(ready, t)
		}
	}
	for len(ready) > 0 {
		t := ready[0]
		ready = ready[1:]
		order = append(order, t)
		kids := make([]uint32, 0, len(children[t]))
		for c := range children[t] {
			kids = append(kids, c)
		}
		sort.Slice(kids, func(i, j int) bool { return indexOf(tables, kids[i]) < indexOf(tables, kids[j]) })
		for _, c := range kids {
			indeg[c]--
			if indeg[c] == 0 {
				ready = append(ready, c)
			}
		}
	}
	if len(order) < len(tables) {
		res.OrderCycle = true
		placed := map[uint32]bool{}
		for _, t := range order {
			placed[t] = true
		}
		for _, t := range tables { // the cycle keeps discovery order
			if !placed[t] {
				order = append(order, t)
			}
		}
	}
	for i := len(order) - 1; i >= 0; i-- {
		res.DeleteOrder = append(res.DeleteOrder, order[i])
	}
}

func indexOf(xs []uint32, x uint32) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return len(xs)
}

func (a *analysis) decide() {
	res := a.res
	switch {
	case len(res.Blockers) > 0:
		res.Verdict = VerdictBlocked
		res.ManualPossible = true
		for _, b := range res.Blockers {
			if !b.Resolvable {
				res.ManualPossible = false
				res.ManualReason = b.Reason
			}
		}
	case res.DeletedRows() > 1 || len(res.Updates) > 0:
		res.Verdict = VerdictCascade
	default:
		res.Verdict = VerdictSafe
	}
	if res.OrderCycle {
		res.alert(LevelInfo, "Affected tables reference each other in a cycle; scripts use one statement (data-modifying CTEs) so FK checks run once at its end.")
	}
}
