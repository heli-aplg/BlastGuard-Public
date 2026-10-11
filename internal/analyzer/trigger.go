package analyzer

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"blastguard/internal/catalog"
	"blastguard/internal/db"
)

// Finding is one observation about a trigger function. Findings never change
// the verdict; they are hints for a human reviewer.
type Finding struct {
	Label string // e.g. "STATIC HINT – UNVERIFIED"
	Kind  string // writes, dynamic-sql, may-abort, may-skip, external, notify, calls, truncate, language
	Text  string
	// Ref is a relation name mentioned by the code; Resolved is filled by the
	// analysis after checking it against the catalog ("" = not found).
	Ref      string
	Resolved string
	Checked  bool
}

type TriggerInput struct {
	Table   *catalog.TableInfo
	Trigger catalog.Trigger
	Event   string
	Source  string
}

// TriggerAnalyzer inspects one trigger. Implementations must be read-only and
// must label every finding as unverified.
type TriggerAnalyzer interface {
	Name() string
	Analyze(ctx context.Context, in TriggerInput) ([]Finding, error)
}

// StaticAnalyzer scans the function source with regular expressions. It cannot
// know what the code does at runtime (dynamic SQL, branches on data, nested
// calls); see docs/SPEC.md §7.6.
type StaticAnalyzer struct{}

func (StaticAnalyzer) Name() string { return "static" }

const staticLabel = "STATIC HINT – UNVERIFIED"

const identRe = `((?:"[^"]+"|[a-z_][a-z0-9_$]*)(?:\.(?:"[^"]+"|[a-z_][a-z0-9_$]*))?)`

var (
	reLineComment  = regexp.MustCompile(`--[^\n]*`)
	reBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	reDelete       = regexp.MustCompile(`(?i)\bdelete\s+from\s+(?:only\s+)?` + identRe)
	reUpdate       = regexp.MustCompile(`(?i)\bupdate\s+(?:only\s+)?` + identRe + `\s+(?:as\s+\w+\s+|\w+\s+)?set\b`)
	reInsert       = regexp.MustCompile(`(?i)\binsert\s+into\s+` + identRe)
	reTruncate     = regexp.MustCompile(`(?i)\btruncate\s+(?:table\s+)?(?:only\s+)?` + identRe)
	reExecute      = regexp.MustCompile(`(?i)\bexecute\s+(\S+)`)
	reRaise        = regexp.MustCompile(`(?i)\braise\s+exception\b`)
	reReturnNull   = regexp.MustCompile(`(?i)\breturn\s+null\b`)
	reDblink       = regexp.MustCompile(`(?i)\b(dblink\w*)\s*\(`)
	reNotify       = regexp.MustCompile(`(?i)\bpg_notify\s*\(|\bnotify\s+\w`)
	rePerform      = regexp.MustCompile(`(?i)\b(?:perform|call)\s+` + identRe + `\s*\(`)
)

// stripComments removes SQL comments (string literals are not parsed; this is
// a hint generator, not a parser).
func stripComments(src string) string {
	return reLineComment.ReplaceAllString(reBlockComment.ReplaceAllString(src, " "), " ")
}

// functionBody keeps the text after the first "AS $tag$" so the CREATE header
// does not produce false matches.
func functionBody(src string) string {
	if i := strings.Index(strings.ToUpper(src), " AS $"); i >= 0 {
		return src[i:]
	}
	return src
}

func normIdent(s string) string {
	parts := strings.Split(s, ".")
	for i, p := range parts {
		if !strings.HasPrefix(p, `"`) {
			parts[i] = strings.ToLower(p)
		}
	}
	return strings.Join(parts, ".")
}

func (StaticAnalyzer) Analyze(_ context.Context, in TriggerInput) ([]Finding, error) {
	tg := in.Trigger
	if l := strings.ToLower(tg.Lang); l != "plpgsql" && l != "sql" {
		return []Finding{{Label: staticLabel, Kind: "language",
			Text: fmt.Sprintf("function language %q cannot be analyzed; behaviour is unknown", tg.Lang)}}, nil
	}
	body := stripComments(functionBody(in.Source))
	var out []Finding
	seen := map[string]bool{}
	add := func(f Finding) {
		k := f.Kind + "|" + f.Text + "|" + f.Ref
		if !seen[k] {
			seen[k] = true
			f.Label = staticLabel
			out = append(out, f)
		}
	}
	for _, m := range reDelete.FindAllStringSubmatch(body, -1) {
		add(Finding{Kind: "writes", Text: "DELETE FROM " + m[1], Ref: normIdent(m[1])})
	}
	for _, m := range reUpdate.FindAllStringSubmatch(body, -1) {
		add(Finding{Kind: "writes", Text: "UPDATE " + m[1], Ref: normIdent(m[1])})
	}
	for _, m := range reInsert.FindAllStringSubmatch(body, -1) {
		add(Finding{Kind: "writes", Text: "INSERT INTO " + m[1], Ref: normIdent(m[1])})
	}
	for _, m := range reTruncate.FindAllStringSubmatch(body, -1) {
		add(Finding{Kind: "truncate", Text: "TRUNCATE " + m[1], Ref: normIdent(m[1])})
	}
	for _, m := range reExecute.FindAllStringSubmatch(body, -1) {
		if w := strings.ToLower(m[1]); w == "function" || w == "procedure" {
			continue
		}
		add(Finding{Kind: "dynamic-sql", Text: "EXECUTE (dynamic SQL): target tables are only known at runtime"})
	}
	if reRaise.MatchString(body) {
		add(Finding{Kind: "may-abort", Text: "RAISE EXCEPTION: may abort the whole DELETE depending on data"})
	}
	if reReturnNull.MatchString(body) {
		if tg.Timing() == "BEFORE" && tg.RowLevel() {
			add(Finding{Kind: "may-skip", Text: "RETURN NULL in a BEFORE ROW trigger: the row may be silently NOT deleted"})
		} else {
			add(Finding{Kind: "may-skip", Text: "RETURN NULL present (no effect on AFTER/STATEMENT triggers)"})
		}
	}
	for _, m := range reDblink.FindAllStringSubmatch(body, -1) {
		add(Finding{Kind: "external", Text: m[1] + "(): opens another connection; its effects are outside this transaction and cannot be rolled back"})
	}
	if reNotify.MatchString(body) {
		add(Finding{Kind: "notify", Text: "NOTIFY / pg_notify: listeners outside the database will be signalled on commit"})
	}
	for _, m := range rePerform.FindAllStringSubmatch(body, -1) {
		add(Finding{Kind: "calls", Text: "calls " + m[1] + "(): nested function behaviour is not analyzed"})
	}
	return out, nil
}

// AIAnalyzer is the planned LLM-backed analyzer (docs/SPEC.md §9).
//
// TODO(ai): implement. Constraints:
//   - every finding labelled "AI ANALYSIS – UNVERIFIED", shown apart from the verdict, never changing it;
//   - agent tools are read-only and go through db.Session (same guards as everything else);
//   - require citations (function + line); cross-check every relation/function name with the catalog;
//   - disabled by default; send DDL and function source only, row data behind a separate opt-in;
//   - provider and model come from config.
type AIAnalyzer struct{}

func (AIAnalyzer) Name() string { return "ai" }

func (AIAnalyzer) Analyze(context.Context, TriggerInput) ([]Finding, error) {
	return nil, fmt.Errorf("AI trigger analyzer is not implemented yet (TODO)")
}

// analyzeTriggers lists triggers that fire for the impacted tables, reads their
// sources, runs the analyzers and raises visibility alerts.
func (a *analysis) analyzeTriggers() error {
	res := a.res
	deleted, updated := a.impacted()

	if privs, err := catalog.ReadonlyCatalogPrivileges(a.ctx, a.s); err == nil {
		if !privs.Trigger {
			res.alert(LevelWarn, "readonly cannot SELECT pg_catalog.pg_trigger; trigger data came from becareful (if available).")
		}
		if !privs.Proc || !privs.Prosrc {
			res.alert(LevelWarn, "readonly cannot read pg_catalog.pg_proc.prosrc; function sources came from becareful (if available).")
		}
	}

	type item struct {
		t     *catalog.TableInfo
		event string
		match func(catalog.Trigger) bool
	}
	var items []item
	for _, t := range deleted {
		items = append(items, item{t, "DELETE", catalog.Trigger.OnDelete})
	}
	for _, t := range updated {
		items = append(items, item{t, "UPDATE", catalog.Trigger.OnUpdate})
	}

	sources := map[uint32]TriggerReport{}
	userTriggers := 0
	firing := 0
	for _, it := range items {
		for _, tg := range it.t.UserTriggers() {
			userTriggers++
			if !it.match(tg) {
				continue
			}
			tr := TriggerReport{Table: it.t, Trigger: tg, Event: it.event}
			if !tg.FuncVisible {
				tr.SourceErr = "function is not visible in pg_proc"
				res.alert(LevelDanger, fmt.Sprintf("Trigger %s on %s calls function OID %d which is not visible; its behaviour cannot be inspected.",
					tg.Name, it.t.Ref.Short(), tg.FuncOID))
			} else if cached, ok := sources[tg.FuncOID]; ok {
				tr.Source, tr.SourceRole, tr.SourceErr = cached.Source, cached.SourceRole, cached.SourceErr
			} else {
				src, role, err := catalog.FunctionSource(a.ctx, a.s, tg.FuncOID)
				switch {
				case err != nil:
					tr.SourceErr = db.ShortErr(err)
					res.alert(LevelDanger, fmt.Sprintf("Cannot read the source of %s (trigger %s on %s): %s", tg.FuncQ(), tg.Name, it.t.Ref.Short(), tr.SourceErr))
				case strings.TrimSpace(src) == "":
					tr.SourceErr = "empty source"
					res.alert(LevelDanger, fmt.Sprintf("Source of %s (trigger %s on %s) is empty or hidden.", tg.FuncQ(), tg.Name, it.t.Ref.Short()))
				}
				tr.Source, tr.SourceRole = src, role
				sources[tg.FuncOID] = tr
			}
			if tr.SourceErr == "" {
				for _, an := range a.opt.Analyzers {
					f, err := an.Analyze(a.ctx, TriggerInput{Table: it.t, Trigger: tg, Event: it.event, Source: tr.Source})
					if err != nil {
						res.alert(LevelWarn, fmt.Sprintf("%s analyzer failed on trigger %s: %v", an.Name(), tg.Name, err))
						continue
					}
					tr.Findings = append(tr.Findings, f...)
				}
			}
			if tg.Fires() {
				firing++
			}
			res.Triggers = append(res.Triggers, tr)
		}
	}
	if err := a.resolveRefs(); err != nil {
		return err
	}

	if firing > 0 {
		res.alert(LevelUnverified, fmt.Sprintf("%d trigger(s) fire during this DELETE; their effects are not evaluated — manual review required.", firing))
	}
	if a.opt.AIEnabled {
		res.alert(LevelInfo, "ai.enabled is set, but the AI trigger analyzer is not implemented yet (TODO).")
	}

	// Cross-check trigger visibility on the target between both accounts.
	target := res.Target
	roCount, roErr := db.CollectOn(a.ctx, a.s, db.Readonly, catalog.QUserTriggerCount, []any{target.Ref.OID}, rowToInt64)
	if a.s.HasBecareful() {
		bcCount, bcErr := db.CollectOn(a.ctx, a.s, db.Becareful, catalog.QUserTriggerCount, []any{target.Ref.OID}, rowToInt64)
		switch {
		case roErr != nil || bcErr != nil:
			res.alert(LevelWarn, fmt.Sprintf("Trigger visibility cross-check on %s failed (readonly: %v, becareful: %v).", target.Ref.Short(), errText(roErr), errText(bcErr)))
		case roCount[0] != bcCount[0]:
			res.alert(LevelDanger, fmt.Sprintf("readonly sees %d trigger(s) on %s but becareful sees %d; some triggers are hidden from readonly.", roCount[0], target.Ref.Short(), bcCount[0]))
		}
	}
	if userTriggers == 0 {
		msg := fmt.Sprintf("No user triggers found on the %d impacted table(s).", len(deleted)+len(updated))
		if a.s.HasBecareful() {
			res.alert(LevelInfo, msg+" Cross-checked with becareful on the target table.")
		} else {
			res.alert(LevelWarn, msg+" becareful is unavailable, so hidden triggers cannot be ruled out.")
		}
	}
	return nil
}

// resolveRefs checks relation names mentioned by findings against the catalog,
// trying the function schema, the table schema and public for unqualified names.
func (a *analysis) resolveRefs() error {
	var names []string
	seen := map[string]bool{}
	candidates := func(tr TriggerReport, ref string) []string {
		if strings.Contains(ref, ".") {
			return []string{ref}
		}
		return []string{tr.Trigger.FuncSchema + "." + ref, tr.Table.Ref.Schema + "." + ref, "public." + ref}
	}
	for _, tr := range a.res.Triggers {
		for _, f := range tr.Findings {
			if f.Ref == "" {
				continue
			}
			for _, c := range candidates(tr, f.Ref) {
				if !seen[c] {
					seen[c] = true
					names = append(names, c)
				}
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	resolved, err := catalog.ResolveRelations(a.ctx, a.s, names)
	if err != nil {
		return err
	}
	for i := range a.res.Triggers {
		tr := &a.res.Triggers[i]
		for j := range tr.Findings {
			f := &tr.Findings[j]
			if f.Ref == "" {
				continue
			}
			f.Checked = true
			for _, c := range candidates(*tr, f.Ref) {
				if rel := resolved[c]; rel != "" {
					f.Resolved = rel
					break
				}
			}
		}
	}
	return nil
}

func errText(err error) string {
	if err == nil {
		return "ok"
	}
	return db.ShortErr(err)
}

func rowToInt64(row pgx.CollectableRow) (int64, error) { return pgx.RowTo[int64](row) }
