// Package analyzer decides what a DELETE of one row would do. It only reads;
// scripts are produced elsewhere from the Result.
package analyzer

import (
	"time"

	"blastguard/internal/catalog"
	"blastguard/internal/db"
)

type Verdict string

const (
	VerdictSafe     Verdict = "SAFE"
	VerdictCascade  Verdict = "CASCADE"
	VerdictBlocked  Verdict = "BLOCKED"
	VerdictNotFound Verdict = "NOT FOUND"
	VerdictDeadEnd  Verdict = "DEAD END"
	VerdictNoPK     Verdict = "UNSUPPORTED"
)

type AlertLevel int

const (
	LevelInfo AlertLevel = iota
	LevelWarn
	LevelUnverified
	LevelDanger
)

type Alert struct {
	Level AlertLevel
	Text  string
}

// Row is one row in the delete set (or the original image of an updated row).
type Row struct {
	Key   string   // PK literals joined, or relname:ctid when the table has no PK
	Lits  []string // literal per column, aligned with TableInfo.Columns
	Depth int      // BFS depth of first discovery (root = 0)
	// Auto means Postgres itself deletes the row when the target is deleted
	// (reached from the target through CASCADE edges only).
	Auto bool
}

// Node is every row of one table that would be deleted.
type Node struct {
	Info  *catalog.TableInfo
	Rows  []*Row
	byKey map[string]*Row
}

type EdgeKind int

const (
	EdgeCascade EdgeKind = iota
	EdgeBlock
	EdgeSetNull
	EdgeSetDefault
)

// Edge is one FK followed from a batch of parent rows.
type Edge struct {
	ID         int
	ParentEdge int // -1 when the parent batch is the target row
	Parent     *catalog.TableInfo
	Child      *catalog.TableInfo
	FK         catalog.IncomingFK
	Kind       EdgeKind
	Depth      int    // depth of the child rows
	Pred       string // predicate on the child table selecting the matched rows
	Count      int64
	Rows       []*Row // matched rows (canonical objects, may already be known)
	NewRows    int
	Source     db.Role
	Unresolved []*Row // blocking rows not removed in the same statement
}

func (e *Edge) Blocking() bool { return e.Kind == EdgeBlock && len(e.Unresolved) > 0 }

// UpdateRow is a child row whose FK columns Postgres would set to NULL/DEFAULT.
type UpdateRow struct {
	Table *catalog.TableInfo
	Row   *Row
	Edge  *Edge
	Cols  []string
}

type Blocker struct {
	Edge       *Edge
	Rows       []*Row
	Reason     string
	Resolvable bool // a manual children-first script can remove it
	Path       []*Edge
}

type TriggerReport struct {
	Table      *catalog.TableInfo
	Trigger    catalog.Trigger
	Event      string // DELETE or UPDATE
	Source     string
	SourceRole db.Role
	SourceErr  string
	Findings   []Finding
}

type Result struct {
	GeneratedAt time.Time
	Target      *catalog.TableInfo
	PKCol       string
	Input       string
	Root        *Row
	RootSource  db.Role
	Verdict     Verdict
	Message     string

	Nodes     map[uint32]*Node
	NodeOrder []uint32 // discovery order
	Updates   []*UpdateRow
	Edges     []*Edge
	Blockers  []Blocker
	Triggers  []TriggerReport
	Alerts    []Alert
	Notes     []string // data-source notes (fallbacks, snapshot)

	DeadEnd *db.DeadEndError

	// DeleteOrder lists tables children-first for a manual script.
	DeleteOrder    []uint32
	OrderCycle     bool // tables reference each other; scripts use one CTE statement
	ManualPossible bool
	ManualReason   string

	SharedSnapshot bool
}

func (r *Result) alert(level AlertLevel, text string) {
	r.Alerts = append(r.Alerts, Alert{Level: level, Text: text})
}

// DeletedRows is the number of rows in the delete set, target included.
func (r *Result) DeletedRows() int {
	n := 0
	for _, node := range r.Nodes {
		n += len(node.Rows)
	}
	return n
}

// PathTo returns the chain of edges leading to e (root first).
func (r *Result) PathTo(e *Edge) []*Edge {
	var path []*Edge
	for cur := e; cur != nil; {
		path = append([]*Edge{cur}, path...)
		if cur.ParentEdge < 0 {
			break
		}
		cur = r.Edges[cur.ParentEdge]
	}
	return path
}

func (r *Result) Unverified() int {
	n := 0
	for _, a := range r.Alerts {
		if a.Level >= LevelUnverified {
			n++
		}
	}
	return n
}
