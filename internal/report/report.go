// Package report renders analysis results and table structures as text. The
// same renderer produces the styled TUI view and the plain copy/exit output.
package report

import (
	"fmt"
	"sort"
	"strings"

	"blastguard/internal/analyzer"
	"blastguard/internal/catalog"
	"blastguard/internal/script"
)

// Styler decorates text. Plain returns it unchanged.
type Styler interface {
	Section(string) string
	Verdict(analyzer.Verdict, string) string
	Danger(string) string
	Warn(string) string
	OK(string) string
	Muted(string) string
	Bold(string) string
	Code(string) string
}

type Plain struct{}

func (Plain) Section(s string) string                     { return s }
func (Plain) Verdict(_ analyzer.Verdict, s string) string { return s }
func (Plain) Danger(s string) string                      { return s }
func (Plain) Warn(s string) string                        { return s }
func (Plain) OK(s string) string                          { return s }
func (Plain) Muted(s string) string                       { return s }
func (Plain) Bold(s string) string                        { return s }
func (Plain) Code(s string) string                        { return s }

func section(b *strings.Builder, st Styler, title string) {
	b.WriteString("\n")
	b.WriteString(st.Section("━━ " + title + " ━━"))
	b.WriteString("\n")
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// Option is a choice offered after an analysis.
type Option struct {
	Key    string
	Label  string
	Action string // "script" | "cancel"
}

// Options returns the choices for a result (spec §8.2).
func Options(res *analyzer.Result) []Option {
	switch res.Verdict {
	case analyzer.VerdictSafe, analyzer.VerdictCascade:
		return []Option{{"1", "Generate delete + restore script", "script"}, {"0", "Cancel", "cancel"}}
	case analyzer.VerdictBlocked:
		if res.ManualPossible {
			return []Option{{"1", "Generate manual cascade script (children first) + restore script", "script"}, {"0", "Cancel", "cancel"}}
		}
	}
	return []Option{{"0", "Back", "cancel"}}
}

// HasScript reports whether an option generates scripts.
func HasScript(res *analyzer.Result) bool {
	for _, o := range Options(res) {
		if o.Action == "script" {
			return true
		}
	}
	return false
}

// Render produces the full analysis report.
func Render(res *analyzer.Result, st Styler) string {
	var b strings.Builder
	t := res.Target

	section(&b, st, "TARGET")
	if t != nil {
		fmt.Fprintf(&b, "%s  %s = %s", st.Bold(t.Ref.Q()), res.PKCol, st.Code(script.RootLiteral(res)))
		if res.Root != nil {
			fmt.Fprintf(&b, "  %s", st.Muted("(row found, read via "+string(res.RootSource)+")"))
		}
		b.WriteString("\n")
	}

	section(&b, st, "VERDICT")
	b.WriteString(st.Verdict(res.Verdict, verdictLine(res)))
	b.WriteString("\n")

	if res.Verdict == analyzer.VerdictDeadEnd && res.DeadEnd != nil {
		section(&b, st, "DEAD END")
		fmt.Fprintf(&b, "%s\n", st.Danger("While "+res.DeadEnd.Purpose+":"))
		for _, r := range []string{"readonly", "becareful"} {
			for role, err := range res.DeadEnd.Errs {
				if string(role) == r {
					fmt.Fprintf(&b, "  %-9s %s\n", r, st.Danger(errString(err)))
				}
			}
		}
		fmt.Fprintf(&b, "  %s\n", st.Muted("query: "+oneLine(res.DeadEnd.SQL)))
	}

	if len(res.Blockers) > 0 {
		section(&b, st, "BLOCKERS")
		for i, bl := range res.Blockers {
			renderBlocker(&b, st, res, i+1, bl)
		}
	}

	if res.Root != nil {
		section(&b, st, "IMPACT TREE")
		renderTree(&b, st, res)
	}

	if len(res.Triggers) > 0 {
		section(&b, st, "TRIGGERS")
		for _, tr := range res.Triggers {
			renderTrigger(&b, st, tr)
		}
	}

	if len(res.Alerts) > 0 {
		section(&b, st, "ALERTS")
		alerts := append([]analyzer.Alert{}, res.Alerts...)
		sort.SliceStable(alerts, func(i, j int) bool { return alerts[i].Level > alerts[j].Level })
		for _, a := range alerts {
			b.WriteString(alertLine(st, a))
			b.WriteString("\n")
		}
	}

	section(&b, st, "DATA SOURCES")
	if res.Root != nil {
		fmt.Fprintf(&b, "  one read-only REPEATABLE READ snapshot on readonly; rolled back after analysis\n")
	}
	if res.SharedSnapshot {
		fmt.Fprintf(&b, "  becareful imported the readonly snapshot (same data view)\n")
	}
	for _, n := range res.Notes {
		fmt.Fprintf(&b, "  %s\n", st.Muted(n))
	}

	if res.Verdict == analyzer.VerdictSafe && res.Root != nil {
		section(&b, st, "ROW DATA (copy to revert)")
		b.WriteString(st.Code(script.Insert(t, []*analyzer.Row{res.Root})))
	}

	if len(res.Triggers) > 0 {
		section(&b, st, "TRIGGER FUNCTION SOURCES")
		seen := map[uint32]bool{}
		for _, tr := range res.Triggers {
			if seen[tr.Trigger.FuncOID] || tr.Source == "" {
				continue
			}
			seen[tr.Trigger.FuncOID] = true
			fmt.Fprintf(&b, "%s %s\n", st.Bold("-- "+tr.Trigger.FuncQ()), st.Muted("(read via "+string(tr.SourceRole)+")"))
			b.WriteString(st.Code(strings.TrimRight(tr.Source, "\n")))
			b.WriteString("\n\n")
		}
	}

	section(&b, st, "OPTIONS")
	for _, o := range Options(res) {
		fmt.Fprintf(&b, "  [%s] %s\n", o.Key, o.Label)
	}
	if res.Verdict == analyzer.VerdictBlocked && !res.ManualPossible && res.ManualReason != "" {
		fmt.Fprintf(&b, "  %s\n", st.Warn("No manual script: "+res.ManualReason+"."))
	}
	return b.String()
}

func verdictLine(res *analyzer.Result) string {
	var s string
	switch res.Verdict {
	case analyzer.VerdictSafe:
		s = "SAFE — no other row references this row; deleting it affects only this row."
	case analyzer.VerdictCascade:
		other, tables := otherRows(res)
		s = fmt.Sprintf("CASCADE — the DELETE succeeds and also deletes %s in %s and updates %s.",
			plural(other, "other row"), plural(tables, "table"), plural(len(res.Updates), "row"))
	case analyzer.VerdictBlocked:
		s = fmt.Sprintf("BLOCKED — %s. ", plural(len(res.Blockers), "blocker"))
		if res.ManualPossible {
			s += "Deleting children first would make it possible (option 1)."
		} else {
			s += "No script can resolve it automatically."
		}
	default:
		s = string(res.Verdict) + " — " + res.Message
	}
	if n := res.Unverified(); n > 0 {
		s += fmt.Sprintf("  ⚠ %d unverified item(s) — see ALERTS.", n)
	}
	return s
}

// otherRows counts deleted rows besides the target and the tables they live in.
func otherRows(res *analyzer.Result) (rows, tables int) {
	for _, n := range res.Nodes {
		k := 0
		for _, r := range n.Rows {
			if r != res.Root {
				k++
			}
		}
		if k > 0 {
			rows += k
			tables++
		}
	}
	return rows, tables
}

func renderBlocker(b *strings.Builder, st Styler, res *analyzer.Result, n int, bl analyzer.Blocker) {
	e := bl.Edge
	fmt.Fprintf(b, "%s %s · FK %s\n", st.Danger(fmt.Sprintf("[%d]", n)), st.Bold(e.Child.Ref.Q()), e.FK.Name)
	fmt.Fprintf(b, "    %s\n", st.Code(e.FK.Def))
	fmt.Fprintf(b, "    path   : %s\n", pathText(res, bl.Path))
	fmt.Fprintf(b, "    reason : %s\n", bl.Reason)
	fmt.Fprintf(b, "    rows   : %d — exact query:\n", len(bl.Rows))
	fmt.Fprintf(b, "    %s\n", st.Code("SELECT * FROM "+e.Child.Ref.Q()+" WHERE "+blockerPred(e, bl.Rows)+";"))
}

// blockerPred selects exactly the blocking rows: the FK predicate when every
// matched row blocks, otherwise their primary keys.
func blockerPred(e *analyzer.Edge, rows []*analyzer.Row) string {
	if len(rows) == len(e.Rows) || len(e.Child.PK) == 0 {
		return e.Pred
	}
	tuples := make([][]string, len(rows))
	for i, r := range rows {
		tuples[i] = analyzer.PKLits(e.Child, r)
	}
	return catalog.InPredicate(e.Child.PK, tuples)
}

func pathText(res *analyzer.Result, path []*analyzer.Edge) string {
	parts := []string{res.Target.Ref.Short()}
	for _, e := range path {
		parts = append(parts, fmt.Sprintf("%s (%s)", e.Child.Ref.Short(), e.FK.ActionName()))
	}
	return strings.Join(parts, " → ")
}

func renderTree(b *strings.Builder, st Styler, res *analyzer.Result) {
	fmt.Fprintf(b, "%s  %s\n", st.Bold(res.Target.Ref.Short()), st.Muted("1 row · TARGET · DELETE"))
	children := map[int][]*analyzer.Edge{}
	var empty []*analyzer.Edge
	for _, e := range res.Edges {
		if e.Count == 0 {
			empty = append(empty, e)
			continue
		}
		children[e.ParentEdge] = append(children[e.ParentEdge], e)
	}
	var walk func(parent int, prefix string)
	walk = func(parent int, prefix string) {
		kids := children[parent]
		for i, e := range kids {
			last := i == len(kids)-1
			branch, next := "├─ ", "│  "
			if last {
				branch, next = "└─ ", "   "
			}
			fmt.Fprintf(b, "%s%s%s\n", prefix, branch, edgeLine(st, e))
			walk(e.ID, prefix+next)
		}
	}
	walk(-1, "")
	if len(empty) > 0 {
		names := make([]string, 0, len(empty))
		for _, e := range empty {
			names = append(names, fmt.Sprintf("%s.%s", e.Child.Ref.Short(), e.FK.Name))
		}
		fmt.Fprintf(b, "%s\n", st.Muted(fmt.Sprintf("checked, no referencing rows (%d): %s", len(empty), strings.Join(names, ", "))))
	}
}

func edgeLine(st Styler, e *analyzer.Edge) string {
	var outcome string
	switch e.Kind {
	case analyzer.EdgeCascade:
		outcome = st.Warn("DELETE")
	case analyzer.EdgeBlock:
		if e.Blocking() {
			outcome = st.Danger("BLOCKS")
		} else {
			outcome = st.OK("resolved (also cascaded)")
		}
	case analyzer.EdgeSetNull:
		outcome = st.Warn("UPDATE " + strings.Join(e.FK.AffectedCols(), ", ") + " → NULL")
	case analyzer.EdgeSetDefault:
		outcome = st.Warn("UPDATE " + strings.Join(e.FK.AffectedCols(), ", ") + " → DEFAULT")
	}
	extra := ""
	if dup := int(e.Count) - e.NewRows; dup > 0 && (e.Kind == analyzer.EdgeCascade || e.Kind == analyzer.EdgeBlock) {
		extra = st.Muted(fmt.Sprintf(" (%d already counted)", dup))
	}
	via := ""
	if e.Source == "becareful" {
		via = st.Muted(" [via becareful]")
	}
	return fmt.Sprintf("%s  via %s  %s  %s  %s%s%s", st.Bold(e.Child.Ref.Short()), e.FK.Name, e.FK.ActionName(),
		plural(int(e.Count), "row"), outcome, extra, via)
}

func renderTrigger(b *strings.Builder, st Styler, tr analyzer.TriggerReport) {
	tg := tr.Trigger
	state := tg.EnabledText()
	if !tg.Fires() {
		state = st.Muted(state + " (does not fire)")
	}
	secdef := ""
	if tg.SecurityDef {
		secdef = " " + st.Warn("SECURITY DEFINER")
	}
	fmt.Fprintf(b, "%s · %s · %s %s %s · %s · fn %s [%s]%s\n", st.Bold(tr.Table.Ref.Short()), st.Bold(tg.Name),
		tg.Timing(), tr.Event, tg.Level(), state, tg.FuncQ(), tg.Lang, secdef)
	fmt.Fprintf(b, "    %s\n", st.Muted(oneLine(tg.Def)))
	if tr.SourceErr != "" {
		fmt.Fprintf(b, "    %s\n", st.Danger("source: "+tr.SourceErr))
	}
	for _, f := range tr.Findings {
		line := fmt.Sprintf("[%s] %s", f.Label, f.Text)
		if f.Checked {
			if f.Resolved != "" {
				line += " → exists as " + f.Resolved
			} else {
				line += " → not found in catalog (CTE, variable, temp or dynamic name?)"
			}
		}
		fmt.Fprintf(b, "    %s\n", st.Warn(line))
	}
	if tr.SourceErr == "" && len(tr.Findings) == 0 {
		fmt.Fprintf(b, "    %s\n", st.Muted("[STATIC HINT – UNVERIFIED] no write/abort/skip patterns found; this does not prove the function is harmless"))
	}
	fmt.Fprintf(b, "    %s\n", st.Muted("TODO(ai): AI analysis not available yet"))
}

func alertLine(st Styler, a analyzer.Alert) string {
	switch a.Level {
	case analyzer.LevelDanger:
		return st.Danger("⛔ " + a.Text)
	case analyzer.LevelUnverified:
		return st.Warn("⚠ UNVERIFIED: " + a.Text)
	case analyzer.LevelWarn:
		return st.Warn("⚠ " + a.Text)
	default:
		return st.Muted("ℹ " + a.Text)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Scripts renders both scripts as one copyable block.
func Scripts(res *analyzer.Result, st Styler) string {
	var b strings.Builder
	section(&b, st, "DELETE SCRIPT  (/copy delete)")
	b.WriteString(st.Code(script.Delete(res)))
	section(&b, st, "RESTORE SCRIPT  (/copy restore)")
	b.WriteString(st.Code(script.Restore(res)))
	b.WriteString("\n" + st.Muted("On-screen text may be wrapped; use /copy to get exact SQL.") + "\n")
	return b.String()
}
