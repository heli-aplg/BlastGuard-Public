package tui

import (
	"github.com/charmbracelet/lipgloss"

	"blastguard/internal/analyzer"
)

var (
	cAccent = lipgloss.AdaptiveColor{Light: "#1d4ed8", Dark: "#7aa2f7"}
	cOK     = lipgloss.AdaptiveColor{Light: "#15803d", Dark: "#9ece6a"}
	cWarn   = lipgloss.AdaptiveColor{Light: "#b45309", Dark: "#e0af68"}
	cDanger = lipgloss.AdaptiveColor{Light: "#b91c1c", Dark: "#f7768e"}
	cMuted  = lipgloss.AdaptiveColor{Light: "#6b7280", Dark: "#737aa2"}
	cCode   = lipgloss.AdaptiveColor{Light: "#0f766e", Dark: "#b4f9f8"}

	sHeaderBox = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cAccent).Padding(0, 1)
	sTitle     = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sOK        = lipgloss.NewStyle().Foreground(cOK)
	sWarn      = lipgloss.NewStyle().Foreground(cWarn)
	sDanger    = lipgloss.NewStyle().Foreground(cDanger).Bold(true)
	sMuted     = lipgloss.NewStyle().Foreground(cMuted)
	sBold      = lipgloss.NewStyle().Bold(true)
	sCode      = lipgloss.NewStyle().Foreground(cCode)
	sSelected  = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sHint      = lipgloss.NewStyle().Foreground(cMuted)
	sCmd       = lipgloss.NewStyle().Foreground(cAccent)
)

// styler implements report.Styler with lipgloss.
type styler struct{}

func (styler) Section(s string) string { return sTitle.Render(s) }
func (styler) Danger(s string) string  { return sDanger.Render(s) }
func (styler) Warn(s string) string    { return sWarn.Render(s) }
func (styler) OK(s string) string      { return sOK.Render(s) }
func (styler) Muted(s string) string   { return sMuted.Render(s) }
func (styler) Bold(s string) string    { return sBold.Render(s) }
func (styler) Code(s string) string    { return sCode.Render(s) }

func (styler) Verdict(v analyzer.Verdict, s string) string {
	switch v {
	case analyzer.VerdictSafe:
		return sOK.Bold(true).Render(s)
	case analyzer.VerdictCascade:
		return sWarn.Bold(true).Render(s)
	default:
		return sDanger.Render(s)
	}
}
