// Package catalog contains every pg_catalog query. information_schema is
// deliberately not used: its trigger and FK views hide rows from accounts that
// only hold SELECT, which would make the analysis silently wrong.
package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"blastguard/internal/db"
)

// QuoteIdent quotes a SQL identifier (same rules as quote_ident, always quoted).
func QuoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

type TableRef struct {
	OID    uint32
	Schema string
	Name   string
}

// Q is the schema-qualified, quoted name.
func (t TableRef) Q() string { return QuoteIdent(t.Schema) + "." + QuoteIdent(t.Name) }

// Short is a readable name without quotes.
func (t TableRef) Short() string { return t.Schema + "." + t.Name }

type SchemaEntry struct {
	Name  string
	Usage bool
}

type TableEntry struct {
	Schema      string
	Name        string
	Kind        string
	EstRows     int64
	PKCols      int
	PKCol       string
	PKType      string
	RowSecurity bool
}

const qSchemas = `
SELECT n.nspname::text, has_schema_privilege(n.oid, 'USAGE')
FROM pg_namespace n
WHERE n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
  AND n.nspname !~ '^pg_(toast_)?temp_'
ORDER BY 1`

const qTables = `
SELECT n.nspname::text, c.relname::text, c.relkind::text, GREATEST(c.reltuples, 0)::bigint,
       COALESCE(pk.cols, 0), COALESCE(pk.col, ''), COALESCE(pk.typ, ''), c.relrowsecurity
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN LATERAL (
    SELECT cardinality(con.conkey) AS cols, a.attname::text AS col,
           format_type(a.atttypid, a.atttypmod) AS typ
    FROM pg_constraint con
    JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = con.conkey[1]
    WHERE con.conrelid = c.oid AND con.contype = 'p'
) pk ON true
WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
  AND n.nspname !~ '^pg_(toast_)?temp_'
ORDER BY 1, 2`

func ListSchemas(ctx context.Context, s *db.Session) ([]SchemaEntry, error) {
	out, _, err := db.Collect(ctx, s, "listing schemas", qSchemas, nil, pgx.RowToStructByPos[SchemaEntry])
	return out, err
}

func ListTables(ctx context.Context, s *db.Session) ([]TableEntry, error) {
	out, _, err := db.Collect(ctx, s, "listing tables", qTables, nil, pgx.RowToStructByPos[TableEntry])
	return out, err
}

// ---- table detail -------------------------------------------------------

type Column struct {
	Num       int16
	Name      string
	Type      string // with typmod, for display
	BaseType  string // regtype without typmod, for casts
	NotNull   bool
	Identity  string // '', 'a' (ALWAYS), 'd' (BY DEFAULT)
	Generated string // '', 's' (stored), 'v' (virtual, PG18+)
	Default   string
}

type Constraint struct {
	OID  uint32
	Name string
	Type string // p u f c x t n
	Def  string
	Cols []string
}

type IncomingFK struct {
	OID          uint32
	Name         string
	ChildOID     uint32
	ChildSchema  string
	ChildName    string
	ChildCols    []string
	ChildNotNull []bool
	ParentCols   []string
	DelAction    string // a no action, r restrict, c cascade, n set null, d set default
	Deferrable   bool
	Deferred     bool
	Def          string
	SetCols      []string // PG15+ ON DELETE SET NULL (cols); empty = all FK columns
}

func (f IncomingFK) Child() TableRef {
	return TableRef{OID: f.ChildOID, Schema: f.ChildSchema, Name: f.ChildName}
}

// ActionName is the SQL spelling of the ON DELETE action.
func (f IncomingFK) ActionName() string {
	switch f.DelAction {
	case "c":
		return "CASCADE"
	case "r":
		return "RESTRICT"
	case "n":
		return "SET NULL"
	case "d":
		return "SET DEFAULT"
	default:
		return "NO ACTION"
	}
}

// AffectedCols are the child columns changed by SET NULL / SET DEFAULT.
func (f IncomingFK) AffectedCols() []string {
	if len(f.SetCols) > 0 {
		return f.SetCols
	}
	return f.ChildCols
}

type Trigger struct {
	OID           uint32
	Name          string
	Enabled       string // O origin, D disabled, R replica, A always
	Type          int32
	Internal      bool
	ConstraintOID uint32
	Def           string
	FuncOID       uint32
	FuncVisible   bool
	FuncSchema    string
	FuncName      string
	Lang          string
	SecurityDef   bool
}

// tgtype bits (src/include/catalog/pg_trigger.h).
const (
	tgRow      = 1 << 0
	tgBefore   = 1 << 1
	tgInsert   = 1 << 2
	tgDelete   = 1 << 3
	tgUpdate   = 1 << 4
	tgTruncate = 1 << 5
	tgInstead  = 1 << 6
)

func (t Trigger) OnDelete() bool { return t.Type&tgDelete != 0 }
func (t Trigger) OnUpdate() bool { return t.Type&tgUpdate != 0 }
func (t Trigger) OnInsert() bool { return t.Type&tgInsert != 0 }
func (t Trigger) RowLevel() bool { return t.Type&tgRow != 0 }

func (t Trigger) Timing() string {
	switch {
	case t.Type&tgInstead != 0:
		return "INSTEAD OF"
	case t.Type&tgBefore != 0:
		return "BEFORE"
	default:
		return "AFTER"
	}
}

func (t Trigger) Level() string {
	if t.RowLevel() {
		return "ROW"
	}
	return "STATEMENT"
}

// Fires reports whether the trigger fires under the default
// session_replication_role (origin).
func (t Trigger) Fires() bool { return t.Enabled == "O" || t.Enabled == "A" }

func (t Trigger) EnabledText() string {
	switch t.Enabled {
	case "D":
		return "disabled"
	case "R":
		return "replica-only"
	case "A":
		return "always"
	default:
		return "enabled"
	}
}

func (t Trigger) FuncQ() string { return t.FuncSchema + "." + t.FuncName + "()" }

type Rule struct {
	Name  string
	Event string // '2' update, '4' delete
	Def   string
}

type TableInfo struct {
	Ref              TableRef
	Kind             string
	RowSecurity      bool
	ForceRowSecurity bool
	IsPartition      bool
	HasInheritors    bool
	Columns          []Column
	PK               []string
	Constraints      []Constraint
	Incoming         []IncomingFK
	Triggers         []Trigger
	Rules            []Rule
	// Sources records which account answered each detail query.
	Sources map[string]db.Role
}

func (t *TableInfo) ColIndex(name string) int {
	for i, c := range t.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

func (t *TableInfo) Column(name string) *Column {
	if i := t.ColIndex(name); i >= 0 {
		return &t.Columns[i]
	}
	return nil
}

func (t *TableInfo) UserTriggers() []Trigger {
	var out []Trigger
	for _, tg := range t.Triggers {
		if !tg.Internal {
			out = append(out, tg)
		}
	}
	return out
}

func (t *TableInfo) KindText() string {
	switch t.Kind {
	case "p":
		return "partitioned table"
	default:
		return "table"
	}
}

type relRow struct {
	OID              uint32
	Schema           string
	Name             string
	Kind             string
	RowSecurity      bool
	ForceRowSecurity bool
	IsPartition      bool
	HasInheritors    bool
}

const qRelBase = `
SELECT c.oid, n.nspname::text, c.relname::text, c.relkind::text, c.relrowsecurity,
       c.relforcerowsecurity, c.relispartition,
       EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhparent = c.oid)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
`

const qColumns = `
SELECT a.attnum, a.attname::text, format_type(a.atttypid, a.atttypmod), a.atttypid::regtype::text,
       a.attnotnull, a.attidentity::text, a.attgenerated::text,
       COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
FROM pg_attribute a
LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`

// colNames maps an int2[] of attnums on a relation to attribute names, in order.
func colNames(arr, rel string) string {
	return fmt.Sprintf(`ARRAY(SELECT a.attname::text FROM unnest(%s) WITH ORDINALITY k(n, ord)
        JOIN pg_attribute a ON a.attrelid = %s AND a.attnum = k.n ORDER BY k.ord)`, arr, rel)
}

var qConstraints = `
SELECT con.oid, con.conname::text, con.contype::text, pg_get_constraintdef(con.oid, true),
       ` + colNames("con.conkey", "con.conrelid") + `
FROM pg_constraint con
WHERE con.conrelid = $1
ORDER BY CASE con.contype WHEN 'p' THEN 0 WHEN 'u' THEN 1 WHEN 'f' THEN 2 ELSE 3 END, con.conname`

func qIncoming(versionNum int) string {
	setCols := `ARRAY[]::text[]`
	if versionNum >= 150000 {
		setCols = colNames("con.confdelsetcols", "con.conrelid")
	}
	return `
SELECT con.oid, con.conname::text, cc.oid, cn.nspname::text, cc.relname::text,
       ` + colNames("con.conkey", "con.conrelid") + `,
       ARRAY(SELECT a.attnotnull FROM unnest(con.conkey) WITH ORDINALITY k(n, ord)
             JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.n ORDER BY k.ord),
       ` + colNames("con.confkey", "con.confrelid") + `,
       con.confdeltype::text, con.condeferrable, con.condeferred, pg_get_constraintdef(con.oid, true),
       ` + setCols + `
FROM pg_constraint con
JOIN pg_class cc ON cc.oid = con.conrelid
JOIN pg_namespace cn ON cn.oid = cc.relnamespace
WHERE con.contype = 'f' AND con.confrelid = $1 AND con.conparentid = 0
ORDER BY 4, 5, 2`
}

const qTriggers = `
SELECT t.oid, t.tgname::text, t.tgenabled::text, t.tgtype::int, t.tgisinternal, t.tgconstraint,
       pg_get_triggerdef(t.oid, true), t.tgfoid, (p.oid IS NOT NULL),
       COALESCE(pn.nspname::text, ''), COALESCE(p.proname::text, ''), COALESCE(l.lanname::text, ''),
       COALESCE(p.prosecdef, false)
FROM pg_trigger t
LEFT JOIN pg_proc p ON p.oid = t.tgfoid
LEFT JOIN pg_namespace pn ON pn.oid = p.pronamespace
LEFT JOIN pg_language l ON l.oid = p.prolang
WHERE t.tgrelid = $1
ORDER BY t.tgisinternal, t.tgname`

// QUserTriggerCount is used to cross-check trigger visibility between accounts.
const QUserTriggerCount = `SELECT count(*) FROM pg_trigger WHERE tgrelid = $1 AND NOT tgisinternal`

const qRules = `
SELECT r.rulename::text, r.ev_type::text, pg_get_ruledef(r.oid, true)
FROM pg_rewrite r
WHERE r.ev_class = $1 AND r.ev_type IN ('2', '4')
ORDER BY 1`

// ErrTableNotFound is returned when a cached table name no longer resolves.
type ErrTableNotFound struct{ Schema, Name string }

func (e ErrTableNotFound) Error() string {
	return fmt.Sprintf("table %s.%s not found (dropped or renamed? try /refresh)", e.Schema, e.Name)
}

// LoadTable loads the full live definition of schema.name.
func LoadTable(ctx context.Context, s *db.Session, schema, name string) (*TableInfo, error) {
	rels, _, err := db.Collect(ctx, s, "resolving table "+schema+"."+name,
		qRelBase+`WHERE n.nspname = $1 AND c.relname = $2`, []any{schema, name}, pgx.RowToStructByPos[relRow])
	if err != nil {
		return nil, err
	}
	if len(rels) == 0 {
		return nil, ErrTableNotFound{schema, name}
	}
	return loadDetail(ctx, s, rels[0])
}

// LoadTableByOID loads the full live definition of a relation by OID.
func LoadTableByOID(ctx context.Context, s *db.Session, oid uint32) (*TableInfo, error) {
	rels, _, err := db.Collect(ctx, s, fmt.Sprintf("resolving relation %d", oid),
		qRelBase+`WHERE c.oid = $1`, []any{oid}, pgx.RowToStructByPos[relRow])
	if err != nil {
		return nil, err
	}
	if len(rels) == 0 {
		return nil, fmt.Errorf("relation %d not found", oid)
	}
	return loadDetail(ctx, s, rels[0])
}

func loadDetail(ctx context.Context, s *db.Session, r relRow) (*TableInfo, error) {
	t := &TableInfo{
		Ref:  TableRef{OID: r.OID, Schema: r.Schema, Name: r.Name},
		Kind: r.Kind, RowSecurity: r.RowSecurity, ForceRowSecurity: r.ForceRowSecurity,
		IsPartition: r.IsPartition, HasInheritors: r.HasInheritors && r.Kind != "p",
		Sources: map[string]db.Role{},
	}
	what := t.Ref.Short()
	var err error
	if t.Columns, t.Sources["columns"], err = db.Collect(ctx, s, "reading columns of "+what,
		qColumns, []any{r.OID}, pgx.RowToStructByPos[Column]); err != nil {
		return nil, err
	}
	if t.Constraints, t.Sources["constraints"], err = db.Collect(ctx, s, "reading constraints of "+what,
		qConstraints, []any{r.OID}, pgx.RowToStructByPos[Constraint]); err != nil {
		return nil, err
	}
	for _, c := range t.Constraints {
		if c.Type == "p" {
			t.PK = c.Cols
		}
	}
	if t.Incoming, t.Sources["incoming FKs"], err = db.Collect(ctx, s, "reading foreign keys referencing "+what,
		qIncoming(s.ServerVersionNum()), []any{r.OID}, pgx.RowToStructByPos[IncomingFK]); err != nil {
		return nil, err
	}
	if t.Triggers, t.Sources["triggers"], err = db.Collect(ctx, s, "reading triggers of "+what,
		qTriggers, []any{r.OID}, pgx.RowToStructByPos[Trigger]); err != nil {
		return nil, err
	}
	if t.Rules, t.Sources["rules"], err = db.Collect(ctx, s, "reading rules of "+what,
		qRules, []any{r.OID}, pgx.RowToStructByPos[Rule]); err != nil {
		return nil, err
	}
	return t, nil
}

// FunctionSource returns the CREATE FUNCTION text, falling back to prosrc.
func FunctionSource(ctx context.Context, s *db.Session, oid uint32) (string, db.Role, error) {
	out, role, err := db.Collect(ctx, s, fmt.Sprintf("reading source of function %d", oid),
		`SELECT COALESCE(pg_get_functiondef($1::oid), '')`, []any{oid}, pgx.RowTo[string])
	if err == nil && len(out) == 1 && out[0] != "" {
		return out[0], role, nil
	}
	out, role, err2 := db.Collect(ctx, s, fmt.Sprintf("reading prosrc of function %d", oid),
		`SELECT COALESCE(prosrc, '') FROM pg_proc WHERE oid = $1`, []any{oid}, pgx.RowTo[string])
	if err2 != nil {
		if err != nil {
			return "", "", err
		}
		return "", "", err2
	}
	if len(out) == 0 {
		return "", role, nil
	}
	return out[0], role, nil
}

// ResolveRelations maps candidate names to the relation they resolve to ("" =
// not found). Names are passed as parameters; to_regclass never raises.
func ResolveRelations(ctx context.Context, s *db.Session, names []string) (map[string]string, error) {
	type pair struct {
		Name string
		Rel  string
	}
	out, _, err := db.Collect(ctx, s, "resolving relation names",
		`SELECT c, COALESCE(to_regclass(c)::text, '') FROM unnest($1::text[]) AS c`,
		[]any{names}, pgx.RowToStructByPos[pair])
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(out))
	for _, p := range out {
		m[p.Name] = p.Rel
	}
	return m, nil
}

// CatalogPrivileges reports whether the readonly account can read the catalog
// columns the trigger analysis depends on.
type CatalogPrivileges struct {
	Trigger bool
	Proc    bool
	Prosrc  bool
}

func ReadonlyCatalogPrivileges(ctx context.Context, s *db.Session) (CatalogPrivileges, error) {
	out, err := db.CollectOn(ctx, s, db.Readonly, `SELECT has_table_privilege('pg_catalog.pg_trigger', 'SELECT'),
		has_table_privilege('pg_catalog.pg_proc', 'SELECT'),
		has_column_privilege('pg_catalog.pg_proc', 'prosrc', 'SELECT')`, nil, pgx.RowToStructByPos[CatalogPrivileges])
	if err != nil || len(out) == 0 {
		return CatalogPrivileges{}, err
	}
	return out[0], nil
}
