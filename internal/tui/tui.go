// Package tui is the interactive part of blastguard: a pinned header, a body
// (list or scrollable report) and a command/input line.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"blastguard/internal/analyzer"
	"blastguard/internal/cache"
	"blastguard/internal/catalog"
	"blastguard/internal/config"
	"blastguard/internal/db"
	"blastguard/internal/i18n"
	"blastguard/internal/report"
	"blastguard/internal/script"
)

type screen int

const (
	scrSchemas screen = iota
	scrTables
	scrRow
)

type rowState int

const (
	rowInput rowState = iota
	rowOption
)

type statusLevel int

const (
	stInfo statusLevel = iota
	stOK
	stWarn
	stErr
)

type item struct {
	key   string // schema or table name
	label string
}

type Model struct {
	cfg  *config.Config
	ro   *db.Account
	bc   *db.Account
	meta *cache.Metadata

	width, height int
	screen        screen
	schema        string
	table         catalog.TableEntry

	items  []item
	filter []int // indexes into items after filtering
	cursor int
	offset int

	input textinput.Model
	vp    viewport.Model
	spin  spinner.Model

	busy   string
	cancel context.CancelFunc
	seq    int

	status      string
	statusLevel statusLevel

	rowState     rowState
	structure    *catalog.TableInfo
	result       *analyzer.Result
	scriptsShown bool

	// last analysis (kept across navigation) for the exit printout.
	lastResult  *analyzer.Result
	lastScripts bool

	// FinalOutput is printed to the normal screen after the TUI exits.
	FinalOutput string
}

// New builds the TUI model. bcErr is shown in the header when becareful is down.
func New(cfg *config.Config, ro, bc *db.Account, meta *cache.Metadata) *Model {
	in := textinput.New()
	in.Prompt = "› "
	in.Focus()
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	m := &Model{cfg: cfg, ro: ro, bc: bc, meta: meta, input: in, spin: sp, vp: viewport.New(80, 20)}
	m.vp.KeyMap = viewport.KeyMap{} // keys are routed explicitly
	m.enterSchemas()
	if cfg.DefaultSchema != "" {
		for _, s := range meta.Schemas {
			if s.Name == cfg.DefaultSchema {
				m.enterTables(s.Name)
			}
		}
	}
	return m
}

func (m *Model) Init() tea.Cmd { return textinput.Blink }

// ---- messages -------------------------------------------------------------

type structureMsg struct {
	seq  int
	info *catalog.TableInfo
	err  error
}

type analysisMsg struct {
	seq int
	res *analyzer.Result
	err error
}

type refreshMsg struct {
	meta *cache.Metadata
	err  error
}

// ---- navigation -----------------------------------------------------------

func (m *Model) setStatus(level statusLevel, format string, args ...any) {
	m.status, m.statusLevel = fmt.Sprintf(format, args...), level
}

func (m *Model) enterSchemas() {
	m.screen = scrSchemas
	m.items = m.items[:0]
	for _, s := range m.meta.Schemas {
		label := s.Name
		if n := len(m.meta.TablesOf(s.Name)); n > 0 {
			label = fmt.Sprintf("%-40s %s", s.Name, sMuted.Render(fmt.Sprintf("%d tables", n)))
		}
		if !s.Usage {
			label += "  " + sWarn.Render(i18n.T("tui.schema.nousage"))
		}
		m.items = append(m.items, item{key: s.Name, label: label})
	}
	m.resetList()
	m.input.Placeholder = i18n.T("tui.placeholder.filter")
}

func (m *Model) enterTables(schema string) {
	m.screen = scrTables
	m.schema = schema
	m.items = m.items[:0]
	tables := m.meta.TablesOf(schema)
	w := 10
	for _, t := range tables {
		w = max(w, len(t.Name))
	}
	w = min(w, 48)
	for _, t := range tables {
		var pk string
		switch t.PKCols {
		case 0:
			pk = sWarn.Render(i18n.T("tui.table.nopk"))
		case 1:
			pk = "PK " + t.PKCol + " " + sMuted.Render(t.PKType)
		default:
			pk = sWarn.Render(i18n.T("tui.table.compositepk"))
		}
		extra := sMuted.Render(fmt.Sprintf("~%d rows", t.EstRows))
		if t.Kind == "p" {
			extra += sMuted.Render(" · partitioned")
		}
		if t.RowSecurity {
			extra += " " + sWarn.Render("RLS")
		}
		m.items = append(m.items, item{key: t.Name, label: fmt.Sprintf("%-*s  %s  %s", w, t.Name, pk, extra)})
	}
	m.resetList()
	m.input.Placeholder = i18n.T("tui.placeholder.filter")
	if len(tables) == 0 {
		m.setStatus(stWarn, "%s", i18n.T("tui.tables.empty", schema))
	}
}

func (m *Model) enterRow(t catalog.TableEntry) tea.Cmd {
	m.screen = scrRow
	m.table = t
	m.rowState = rowInput
	m.structure, m.result, m.scriptsShown = nil, nil, false
	m.input.SetValue("")
	m.input.Placeholder = i18n.T("tui.placeholder.rowid")
	m.refreshContent()
	return m.startStructure()
}

func (m *Model) resetList() {
	m.input.SetValue("")
	m.cursor, m.offset = 0, 0
	m.applyFilter()
}

func (m *Model) applyFilter() {
	q := strings.ToLower(strings.TrimSpace(m.input.Value()))
	if strings.HasPrefix(q, "/") {
		q = ""
	}
	m.filter = m.filter[:0]
	for i, it := range m.items {
		if q == "" || strings.Contains(strings.ToLower(it.key), q) {
			m.filter = append(m.filter, i)
		}
	}
	if m.cursor >= len(m.filter) {
		m.cursor = max(0, len(m.filter)-1)
	}
	m.clampOffset()
}

func (m *Model) back() tea.Cmd {
	if m.busy == "" {
		m.status = ""
	}
	switch m.screen {
	case scrRow:
		if m.busy != "" && m.cancel != nil {
			m.cancel()
			m.busy = ""
			m.seq++
			m.setStatus(stWarn, "%s", i18n.T("tui.cancelled"))
			return nil
		}
		m.enterTables(m.schema)
		m.selectKey(m.table.Name)
	case scrTables:
		m.enterSchemas()
		m.selectKey(m.schema)
	case scrSchemas:
		m.input.SetValue("")
		m.applyFilter()
	}
	return nil
}

func (m *Model) selectKey(key string) {
	for i, idx := range m.filter {
		if m.items[idx].key == key {
			m.cursor = i
			m.clampOffset()
			return
		}
	}
}

// ---- async operations -----------------------------------------------------

func (m *Model) ensure(ctx context.Context) error {
	if err := m.ro.Ensure(ctx, m.cfg.Timeouts.Connect); err != nil {
		return fmt.Errorf("readonly reconnect failed: %w", err)
	}
	if m.bc != nil {
		_ = m.bc.Ensure(ctx, m.cfg.Timeouts.Connect) // becareful stays optional
	}
	return nil
}

func (m *Model) begin(label string) (context.Context, int) {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.busy = label
	m.seq++
	return ctx, m.seq
}

func (m *Model) startStructure() tea.Cmd {
	ctx, seq := m.begin(i18n.T("tui.busy.structure"))
	schema, name := m.schema, m.table.Name
	return tea.Batch(m.spin.Tick, func() tea.Msg {
		if err := m.ensure(ctx); err != nil {
			return structureMsg{seq: seq, err: err}
		}
		s, err := db.Begin(ctx, m.ro, m.bc)
		if err != nil {
			return structureMsg{seq: seq, err: err}
		}
		defer s.Close(context.Background())
		info, err := catalog.LoadTable(ctx, s, schema, name)
		return structureMsg{seq: seq, info: info, err: err}
	})
}

func (m *Model) startAnalysis(raw string) tea.Cmd {
	ctx, seq := m.begin(i18n.T("tui.busy.analyze"))
	opt := analyzer.Options{MaxRows: m.cfg.MaxRows, AIEnabled: m.cfg.AI.Enabled}
	schema, name := m.schema, m.table.Name
	return tea.Batch(m.spin.Tick, func() tea.Msg {
		if err := m.ensure(ctx); err != nil {
			return analysisMsg{seq: seq, err: err}
		}
		res, err := analyzer.Analyze(ctx, m.ro, m.bc, opt, schema, name, raw)
		return analysisMsg{seq: seq, res: res, err: err}
	})
}

func (m *Model) startRefresh() tea.Cmd {
	ctx, _ := m.begin(i18n.T("tui.busy.refresh"))
	return tea.Batch(m.spin.Tick, func() tea.Msg {
		if err := m.ensure(ctx); err != nil {
			return refreshMsg{err: err}
		}
		meta, err := cache.Scan(ctx, m.cfg.CacheDir, m.ro, m.bc)
		return refreshMsg{meta: meta, err: err}
	})
}

// ---- update ----------------------------------------------------------------

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = max(10, msg.Width-4)
		m.layout()
		return m, nil

	case spinner.TickMsg:
		if m.busy == "" {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case structureMsg:
		if msg.seq != m.seq {
			return m, nil
		}
		m.busy = ""
		if msg.err != nil {
			m.setStatus(stErr, "%s", errMessage(msg.err))
			return m, nil
		}
		m.structure = msg.info
		if len(msg.info.PK) != 1 {
			m.setStatus(stWarn, "%s", i18n.T("tui.row.unsupported"))
		} else {
			m.setStatus(stInfo, "%s", i18n.T("tui.row.ready", msg.info.PK[0]))
		}
		m.refreshContent()
		return m, nil

	case analysisMsg:
		if msg.seq != m.seq {
			return m, nil
		}
		m.busy = ""
		if msg.err != nil {
			m.setStatus(stErr, "%s", errMessage(msg.err))
			return m, nil
		}
		m.result, m.scriptsShown = msg.res, false
		m.lastResult, m.lastScripts = msg.res, false
		if report.HasScript(msg.res) {
			m.rowState = rowOption
			m.input.Placeholder = i18n.T("tui.placeholder.option")
		} else {
			m.rowState = rowInput
		}
		m.setStatus(verdictLevel(msg.res.Verdict), "%s", i18n.T("tui.analysis.done", msg.res.Verdict))
		m.refreshContent()
		m.vp.GotoTop()
		return m, nil

	case refreshMsg:
		m.busy = ""
		if msg.err != nil {
			m.setStatus(stErr, "%s", errMessage(msg.err))
			return m, nil
		}
		m.meta = msg.meta
		switch m.screen {
		case scrSchemas:
			m.enterSchemas()
		case scrTables:
			m.enterTables(m.schema)
		}
		m.setStatus(stOK, "%s", i18n.T("tui.refreshed", len(m.meta.Schemas), len(m.meta.Tables)))
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyCtrlC:
		return m, m.quit()
	case tea.KeyEsc:
		return m, m.back()
	case tea.KeyEnter:
		return m.submit()
	case tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown, tea.KeyHome, tea.KeyEnd:
		m.navigate(k.Type)
		return m, nil
	}
	if m.screen == scrRow && m.busy != "" {
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	if m.screen != scrRow {
		m.cursor, m.offset = 0, 0
		m.applyFilter()
	}
	return m, cmd
}

func (m *Model) navigate(t tea.KeyType) {
	if m.screen == scrRow {
		switch t {
		case tea.KeyUp:
			m.vp.ScrollUp(1)
		case tea.KeyDown:
			m.vp.ScrollDown(1)
		case tea.KeyPgUp:
			m.vp.PageUp()
		case tea.KeyPgDown:
			m.vp.PageDown()
		case tea.KeyHome:
			m.vp.GotoTop()
		case tea.KeyEnd:
			m.vp.GotoBottom()
		}
		return
	}
	page := max(1, m.bodyHeight()-1)
	switch t {
	case tea.KeyUp:
		m.cursor--
	case tea.KeyDown:
		m.cursor++
	case tea.KeyPgUp:
		m.cursor -= page
	case tea.KeyPgDown:
		m.cursor += page
	case tea.KeyHome:
		m.cursor = 0
	case tea.KeyEnd:
		m.cursor = len(m.filter) - 1
	}
	m.cursor = max(0, min(m.cursor, len(m.filter)-1))
	m.clampOffset()
}

func (m *Model) clampOffset() {
	h := max(1, m.bodyHeight())
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	m.offset = max(0, m.offset)
}

func (m *Model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if strings.HasPrefix(text, "/") {
		m.input.SetValue("")
		if m.screen != scrRow {
			m.applyFilter()
		}
		return m, m.command(text)
	}
	switch m.screen {
	case scrSchemas:
		if len(m.filter) == 0 {
			m.setStatus(stWarn, "%s", i18n.T("tui.nomatch"))
			return m, nil
		}
		m.status = ""
		m.enterTables(m.items[m.filter[m.cursor]].key)
	case scrTables:
		if len(m.filter) == 0 {
			m.setStatus(stWarn, "%s", i18n.T("tui.nomatch"))
			return m, nil
		}
		name := m.items[m.filter[m.cursor]].key
		for _, t := range m.meta.TablesOf(m.schema) {
			if t.Name == name {
				m.status = ""
				return m, m.enterRow(t)
			}
		}
	case scrRow:
		if m.busy != "" {
			return m, nil
		}
		if m.rowState == rowOption {
			return m, m.chooseOption(text)
		}
		if m.structure == nil {
			m.setStatus(stWarn, "%s", i18n.T("tui.row.notloaded"))
			return m, nil
		}
		if len(m.structure.PK) != 1 {
			m.setStatus(stWarn, "%s", i18n.T("tui.row.unsupported"))
			return m, nil
		}
		if text == "" {
			return m, nil
		}
		m.input.SetValue("")
		return m, m.startAnalysis(text)
	}
	return m, nil
}

func (m *Model) chooseOption(key string) tea.Cmd {
	m.input.SetValue("")
	for _, o := range report.Options(m.result) {
		if o.Key != key {
			continue
		}
		m.rowState = rowInput
		m.input.Placeholder = i18n.T("tui.placeholder.rowid")
		if o.Action == "script" {
			m.scriptsShown, m.lastScripts = true, true
			m.refreshContent()
			m.vp.GotoBottom()
			m.setStatus(stOK, "%s", i18n.T("tui.scripts.ready"))
		} else {
			m.setStatus(stInfo, "%s", i18n.T("tui.option.cancelled"))
		}
		return nil
	}
	m.setStatus(stWarn, "%s", i18n.T("tui.option.invalid"))
	return nil
}

func (m *Model) command(text string) tea.Cmd {
	fields := strings.Fields(text)
	cmd, arg := strings.ToLower(fields[0]), ""
	if len(fields) > 1 {
		arg = strings.ToLower(fields[1])
	}
	if m.busy != "" && cmd != "/exit" && cmd != "/quit" && cmd != "/back" {
		m.setStatus(stWarn, "%s", i18n.T("tui.busy.wait"))
		return nil
	}
	switch cmd {
	case "/exit", "/quit":
		return m.quit()
	case "/back":
		return m.back()
	case "/schema":
		m.status = ""
		m.enterSchemas()
		m.selectKey(m.schema)
	case "/table":
		if m.schema == "" {
			m.setStatus(stWarn, "%s", i18n.T("tui.table.noschema"))
			return nil
		}
		m.status = ""
		m.enterTables(m.schema)
		m.selectKey(m.table.Name)
	case "/refresh":
		return m.startRefresh()
	case "/copy":
		m.copy(arg)
	case "/help":
		m.setStatus(stInfo, "%s", i18n.T("tui.help"))
	default:
		m.setStatus(stWarn, "%s", i18n.T("tui.unknown", cmd))
	}
	return nil
}

func (m *Model) copy(what string) {
	if m.result == nil {
		m.setStatus(stWarn, "%s", i18n.T("tui.copy.nothing"))
		return
	}
	var text string
	switch what {
	case "", "report":
		text = report.Render(m.result, report.Plain{})
		what = "report"
	case "delete", "restore":
		if !m.scriptsShown {
			m.setStatus(stWarn, "%s", i18n.T("tui.copy.noscript"))
			return
		}
		if what == "delete" {
			text = script.Delete(m.result)
		} else {
			text = script.Restore(m.result)
		}
	default:
		m.setStatus(stWarn, "%s", i18n.T("tui.copy.usage"))
		return
	}
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		m.setStatus(stErr, "%s", i18n.T("tui.copy.failed", err))
		return
	}
	m.setStatus(stOK, "%s", i18n.T("tui.copy.done", what, strings.Count(text, "\n")))
}

func (m *Model) quit() tea.Cmd {
	if m.cancel != nil {
		m.cancel()
	}
	if m.lastResult != nil {
		out := report.Render(m.lastResult, report.Plain{})
		if m.lastScripts {
			out += report.Scripts(m.lastResult, report.Plain{})
		}
		m.FinalOutput = out
	}
	return tea.Quit
}

// ---- rendering --------------------------------------------------------------

func (m *Model) headerLines() []string {
	roState := sOK.Render("✓") + " " + m.ro.Label()
	if !m.ro.Connected() {
		roState = sDanger.Render("✗ disconnected")
	}
	bcState := sWarn.Render("⚠ " + i18n.T("tui.header.bcdown"))
	if m.bc.Connected() {
		bcState = sOK.Render("✓") + " " + m.bc.Label()
	}
	line1 := fmt.Sprintf("%s · readonly %s · becareful %s · %s",
		sTitle.Render("BlastGuard"), roState, bcState,
		sMuted.Render(i18n.T("tui.header.cache", m.meta.ScannedAt.Format("2006-01-02 15:04"))))

	crumbs := []string{m.ro.Database}
	if m.screen >= scrTables {
		crumbs = append(crumbs, m.schema)
	}
	if m.screen == scrRow {
		pk := m.table.PKCol + " " + m.table.PKType
		if m.structure != nil && len(m.structure.PK) == 1 {
			pk = m.structure.PK[0] + " " + m.structure.Column(m.structure.PK[0]).Type
		} else if m.table.PKCols != 1 {
			pk = i18n.T("tui.table.unsupportedpk")
		}
		crumbs = append(crumbs, sBold.Render(m.table.Name)+" "+sMuted.Render("(PK "+pk+")"))
	}
	line2 := strings.Join(crumbs, sMuted.Render(" › "))

	var hints []string
	switch m.screen {
	case scrSchemas:
		hints = []string{"↑↓ select", "type to filter", "Enter open", "/refresh", "/help", "/exit"}
	case scrTables:
		hints = []string{"↑↓ select", "type to filter", "Enter open", "Esc·/back", "/schema", "/refresh", "/exit"}
	case scrRow:
		if m.rowState == rowOption {
			hints = []string{"type option number", "↑↓ PgUp/PgDn scroll", "Esc·/back → tables", "/copy [report|delete|restore]", "/exit"}
		} else {
			hints = []string{"Enter analyze", "↑↓ PgUp/PgDn scroll", "Esc·/back → tables", "/table", "/schema", "/copy [report|delete|restore]", "/refresh", "/exit"}
		}
	}
	for i, h := range hints {
		if strings.HasPrefix(h, "/") || strings.Contains(h, "/back") {
			hints[i] = sCmd.Render(h)
		} else {
			hints[i] = sHint.Render(h)
		}
	}
	line3 := strings.Join(hints, sMuted.Render(" · "))
	return []string{line1, line2, line3}
}

func (m *Model) renderHeader() string {
	w := max(20, m.width-2)
	inner := w - 2 // padding
	lines := m.headerLines()
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, inner, "…")
	}
	return sHeaderBox.Width(w).Render(strings.Join(lines, "\n"))
}

func (m *Model) headerHeight() int { return lipgloss.Height(m.renderHeader()) }

func (m *Model) bodyHeight() int {
	if m.height == 0 {
		return 20
	}
	return max(3, m.height-m.headerHeight()-2)
}

func (m *Model) layout() {
	m.vp.Width = max(10, m.width)
	m.vp.Height = m.bodyHeight()
	m.refreshContent()
	m.clampOffset()
}

func (m *Model) refreshContent() {
	var content string
	st := styler{}
	switch {
	case m.result != nil:
		content = report.Render(m.result, st)
		if m.scriptsShown {
			content += report.Scripts(m.result, st)
		}
	case m.structure != nil:
		content = report.Structure(m.structure, m.table.EstRows, st)
	default:
		content = sMuted.Render(i18n.T("tui.busy.structure"))
	}
	m.vp.SetContent(ansi.Wrap(content, max(10, m.vp.Width-1), ""))
}

func (m *Model) View() string {
	if m.width == 0 {
		return ""
	}
	var body string
	if m.screen == scrRow {
		body = m.vp.View()
	} else {
		body = m.renderList()
	}
	status := m.renderStatus()
	prompt := m.input
	switch {
	case m.screen != scrRow:
		prompt.Prompt = "filter› "
	case m.rowState == rowOption:
		prompt.Prompt = "option› "
	default:
		prompt.Prompt = "Row ID› "
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.renderHeader(), body, status, prompt.View())
}

func (m *Model) renderList() string {
	h := m.bodyHeight()
	lines := make([]string, 0, h)
	if len(m.filter) == 0 {
		lines = append(lines, sMuted.Render("  "+i18n.T("tui.nomatch")))
	}
	for i := m.offset; i < len(m.filter) && len(lines) < h; i++ {
		it := m.items[m.filter[i]]
		line := "  " + it.label
		if i == m.cursor {
			line = sSelected.Render("› ") + it.label
		}
		lines = append(lines, ansi.Truncate(line, max(10, m.width-1), "…"))
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderStatus() string {
	if m.busy != "" {
		return m.spin.View() + " " + m.busy + sMuted.Render("  (Esc to cancel)")
	}
	var s string
	switch m.statusLevel {
	case stOK:
		s = sOK.Render(m.status)
	case stWarn:
		s = sWarn.Render(m.status)
	case stErr:
		s = sDanger.Render(m.status)
	default:
		s = sMuted.Render(m.status)
	}
	if m.screen != scrRow && len(m.items) > 0 {
		s = sMuted.Render(fmt.Sprintf("%d/%d  ", len(m.filter), len(m.items))) + s
	}
	return ansi.Truncate(s, max(10, m.width-1), "…")
}

func verdictLevel(v analyzer.Verdict) statusLevel {
	switch v {
	case analyzer.VerdictSafe:
		return stOK
	case analyzer.VerdictCascade:
		return stWarn
	default:
		return stErr
	}
}

func errMessage(err error) string {
	var dead *db.DeadEndError
	var in *analyzer.InputError
	switch {
	case errors.As(err, &in):
		return in.Msg
	case errors.As(err, &dead):
		return "DEAD END: " + dead.Error()
	case errors.Is(err, context.Canceled):
		return i18n.T("tui.cancelled")
	}
	return err.Error()
}

// Run starts the TUI in the alternate screen and returns the final output.
func Run(m *Model) (string, error) {
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return m.FinalOutput, err
}
