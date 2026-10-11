// Package bootstrap runs the plain-console startup: language, tunnels,
// connections and metadata. It runs before the TUI so ssh can prompt for
// passwords on the real terminal.
package bootstrap

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"blastguard/internal/cache"
	"blastguard/internal/config"
	"blastguard/internal/db"
	"blastguard/internal/i18n"
	"blastguard/internal/tunnel"
)

type Result struct {
	RO     *db.Account
	BC     *db.Account
	BCErr  error
	Meta   *cache.Metadata
	Alerts []string
	Tunnel *tunnel.Tunnel // nil unless BlastGuard started it
}

// Close releases the connections and stops a tunnel BlastGuard started.
func (r *Result) Close() {
	if r == nil {
		return
	}
	r.RO.Close()
	r.BC.Close()
	r.Tunnel.Close()
}

var (
	okStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	badStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	dimStyle  = lipgloss.NewStyle().Faint(true)
	boldStyle = lipgloss.NewStyle().Bold(true)
)

type console struct {
	in  *bufio.Reader
	out io.Writer
}

func (c *console) printf(format string, args ...any) { fmt.Fprintf(c.out, format, args...) }

func (c *console) ask(prompt string) string {
	c.printf("%s", prompt)
	line, _ := c.in.ReadString('\n')
	return strings.TrimSpace(line)
}

// Run performs the startup sequence. A returned error means the tool must exit.
func Run(ctx context.Context, cfg *config.Config) (res *Result, err error) {
	c := &console{in: bufio.NewReader(os.Stdin), out: os.Stdout}
	res = &Result{}
	defer func() {
		if err != nil {
			res.Close()
			res = nil
		}
	}()

	c.printf("%s\n", boldStyle.Render(i18n.T("app.title")))
	c.printf("%s\n\n", dimStyle.Render(i18n.T("boot.config", cfg.Path)))

	// 1. Language
	c.printf("%s\n", boldStyle.Render(i18n.T("boot.language")))
	for i, l := range i18n.Languages {
		c.printf("  [%d] %s\n", i+1, l.Name)
	}
	for {
		a := c.ask(i18n.T("boot.language.prompt"))
		if a == "" {
			a = "1"
		}
		n, err := strconv.Atoi(a)
		if err == nil && n >= 1 && n <= len(i18n.Languages) {
			i18n.Set(i18n.Languages[n-1].Code)
			break
		}
		c.printf("%s\n", warnStyle.Render(i18n.T("boot.language.invalid")))
	}

	// 2. Tunnel (shared by both connections)
	c.printf("\n%s\n", boldStyle.Render(i18n.T("boot.tunnels")))
	res.Tunnel = c.checkTunnel(ctx, cfg)

	// 3. Connections
	c.printf("\n%s\n", boldStyle.Render(i18n.T("boot.connect")))
	if cfg.Connections.Becareful == nil {
		c.printf("  %-9s %s\n", config.RoleBecareful, warnStyle.Render(i18n.T("boot.bc.notconfigured")))
	}
	ro, err := db.Connect(ctx, db.Readonly, cfg.Connections.Readonly, cfg.Timeouts)
	if err != nil {
		c.printf("  %-9s %s\n", config.RoleReadonly, badStyle.Render("✗ "+err.Error()))
		return nil, fmt.Errorf("%s", i18n.T("boot.ro.failed"))
	}
	res.RO = ro
	c.printf("  %-9s %s %s\n", config.RoleReadonly, okStyle.Render("✓"), i18n.T("boot.connected", ro.Label(), ro.ServerVersion))
	if ro.VersionNum < 120000 {
		res.Alerts = append(res.Alerts, i18n.T("boot.oldserver", ro.ServerVersion))
	}

	if cfg.Connections.Becareful != nil {
		bc, err := db.Connect(ctx, db.Becareful, cfg.Connections.Becareful, cfg.Timeouts)
		if err != nil {
			res.BCErr = err
			c.printf("  %-9s %s\n", config.RoleBecareful, warnStyle.Render("⚠ "+err.Error()))
			res.Alerts = append(res.Alerts, i18n.T("boot.bc.failed"))
		} else {
			res.BC = bc
			c.printf("  %-9s %s %s\n", config.RoleBecareful, okStyle.Render("✓"), i18n.T("boot.connected", bc.Label(), bc.ServerVersion))
		}
	} else {
		res.BCErr = db.ErrNoBecareful
		res.Alerts = append(res.Alerts, i18n.T("boot.bc.failed"))
	}

	// 4. Metadata
	c.printf("\n%s\n", boldStyle.Render(i18n.T("boot.metadata")))
	if m := cache.Load(cfg.CacheDir, ro); m != nil {
		res.Meta = m
		c.printf("  %s\n", i18n.T("boot.cache.loaded", m.ScannedAt.Format("2006-01-02 15:04"), len(m.Schemas), len(m.Tables)))
	} else {
		c.printf("  %s\n", i18n.T("boot.cache.scanning"))
		m, err := cache.Scan(ctx, cfg.CacheDir, ro, res.BC)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", i18n.T("boot.scan.failed"), err)
		}
		res.Meta = m
		c.printf("  %s\n", i18n.T("boot.cache.scanned", len(m.Schemas), len(m.Tables)))
	}

	if len(res.Alerts) > 0 {
		c.printf("\n%s\n", warnStyle.Render(i18n.T("boot.alerts")))
		for _, a := range res.Alerts {
			c.printf("  %s\n", warnStyle.Render("⚠ "+a))
		}
		c.ask("\n" + i18n.T("boot.continue"))
	}
	return res, nil
}

// checkTunnel makes sure every configured connection's port is listening,
// offering to start the shared tunnel once if one is closed.
func (c *console) checkTunnel(ctx context.Context, cfg *config.Config) *tunnel.Tunnel {
	var addrs []string
	for _, conn := range []*config.Connection{cfg.Connections.Readonly, cfg.Connections.Becareful} {
		if conn != nil && !slices.Contains(addrs, conn.Addr()) {
			addrs = append(addrs, conn.Addr())
		}
	}
	closed := false
	for _, addr := range addrs {
		if tunnel.Listening(addr) {
			c.printf("  %s %s\n", addr, okStyle.Render("✓ "+i18n.T("boot.port.open")))
		} else {
			closed = true
			c.printf("  %s %s\n", addr, badStyle.Render("✗ "+i18n.T("boot.port.closed")))
		}
	}
	if !closed {
		return nil
	}
	if cfg.Tunnel == nil || strings.TrimSpace(cfg.Tunnel.Command) == "" {
		c.printf("  %s\n", warnStyle.Render(i18n.T("boot.tunnel.none")))
		return nil
	}
	c.printf("  %s %s\n", i18n.T("boot.tunnel.command"), dimStyle.Render(cfg.Tunnel.Command))
	if !*cfg.Tunnel.AutoStart {
		c.printf("  %s\n", warnStyle.Render(i18n.T("boot.tunnel.disabled")))
		return nil
	}
	c.printf("  %s\n", i18n.T("boot.tunnel.starting"))
	var t *tunnel.Tunnel
	for _, addr := range addrs {
		if tunnel.Listening(addr) {
			continue
		}
		if t != nil { // the tunnel is up but does not forward this port
			c.printf("  %s\n", badStyle.Render("✗ "+i18n.T("boot.tunnel.notforwarded", addr)))
			continue
		}
		var err error
		if t, err = tunnel.Start(ctx, cfg.Tunnel.Command, addr, cfg.Tunnel.Wait); err != nil {
			c.printf("  %s\n", badStyle.Render("✗ "+err.Error()))
			return nil
		}
		c.printf("  %s\n", okStyle.Render("✓ "+i18n.T("boot.tunnel.up", addr)))
	}
	if t != nil {
		c.printf("  %s\n", dimStyle.Render(i18n.T("boot.tunnel.owned")))
	}
	return t
}
