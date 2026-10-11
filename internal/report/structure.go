package report

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"blastguard/internal/catalog"
)

var constraintLabel = map[string]string{
	"p": "PK", "u": "UQ", "f": "FK", "c": "CK", "x": "EX", "t": "TG", "n": "NN",
}

// Structure renders the live DDL summary of a table (columns, constraints,
// incoming FKs, triggers, rules) shown before a Row ID is entered.
func Structure(t *catalog.TableInfo, estRows int64, st Styler) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · %s · ~%d rows (planner estimate)\n", st.Bold(t.Ref.Q()), t.KindText(), estRows)
	switch len(t.PK) {
	case 0:
		fmt.Fprintf(&b, "%s\n", st.Danger("PRIMARY KEY: none — Row ID input disabled"))
	case 1:
		fmt.Fprintf(&b, "PRIMARY KEY: %s (%s)\n", st.Bold(t.PK[0]), t.Column(t.PK[0]).Type)
	default:
		fmt.Fprintf(&b, "%s\n", st.Danger("PRIMARY KEY: composite ("+strings.Join(t.PK, ", ")+") — Row ID input disabled"))
	}

	section(&b, st, "COLUMNS")
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	for _, c := range t.Columns {
		var flags []string
		if c.NotNull {
			flags = append(flags, "NOT NULL")
		}
		switch c.Identity {
		case "a":
			flags = append(flags, "identity always")
		case "d":
			flags = append(flags, "identity by default")
		}
		switch c.Generated {
		case "s":
			flags = append(flags, "generated stored")
		case "v":
			flags = append(flags, "generated virtual")
		}
		if c.Default != "" && c.Generated == "" {
			flags = append(flags, "default "+c.Default)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", c.Name, c.Type, strings.Join(flags, " · "))
	}
	tw.Flush()

	section(&b, st, "CONSTRAINTS")
	if len(t.Constraints) == 0 {
		b.WriteString(st.Muted("  none") + "\n")
	}
	tw = tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	for _, c := range t.Constraints {
		label := constraintLabel[c.Type]
		if label == "" {
			label = c.Type
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", label, c.Name, oneLine(c.Def))
	}
	tw.Flush()

	section(&b, st, "REFERENCED BY (incoming FKs)")
	if len(t.Incoming) == 0 {
		b.WriteString(st.Muted("  none — no other table references this table") + "\n")
	}
	tw = tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	for _, f := range t.Incoming {
		fmt.Fprintf(tw, "  %s(%s)\t%s\tON DELETE %s\n", f.Child().Short(), strings.Join(f.ChildCols, ", "), f.Name, f.ActionName())
	}
	tw.Flush()

	section(&b, st, "TRIGGERS")
	user := t.UserTriggers()
	if len(user) == 0 {
		b.WriteString(st.Muted("  none visible") + "\n")
	}
	for _, tg := range user {
		fmt.Fprintf(&b, "  %s · %s · fn %s [%s]\n    %s\n", st.Bold(tg.Name), tg.EnabledText(), tg.FuncQ(), tg.Lang, st.Muted(oneLine(tg.Def)))
	}
	if len(t.Rules) > 0 {
		section(&b, st, "RULES")
		for _, r := range t.Rules {
			fmt.Fprintf(&b, "  %s %s\n", st.Bold(r.Name), st.Muted(oneLine(r.Def)))
		}
	}

	var alerts []string
	if t.RowSecurity {
		alerts = append(alerts, "row-level security is enabled: counts may exclude rows hidden from the reading account")
	}
	if t.HasInheritors {
		alerts = append(alerts, "table has inheritance children")
	}
	var viaBC []string
	for what, role := range t.Sources {
		if role == "becareful" {
			viaBC = append(viaBC, what)
		}
	}
	sort.Strings(viaBC)
	for _, what := range viaBC {
		alerts = append(alerts, what+" were read via becareful (readonly lacked privilege or timed out)")
	}
	if len(alerts) > 0 {
		section(&b, st, "ALERTS")
		for _, a := range alerts {
			fmt.Fprintf(&b, "  %s\n", st.Warn("⚠ "+a))
		}
	}
	return b.String()
}
