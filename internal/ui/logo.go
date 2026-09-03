package ui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	logoShineInterval = 10 * time.Second
	logoShineDelay    = 2 * time.Second
	logoShineTickRate = 50 * time.Millisecond
	logoShineStep     = 2
	logoShineBand     = 4
)

var (
	logoBaseStyle  = lipgloss.NewStyle().Foreground(Colors.LightText)
	logoShineStyle = lipgloss.NewStyle().Foreground(Colors.FocusOrange)
)

func rebuildLogoStyles() {
	logoBaseStyle = lipgloss.NewStyle().Foreground(Colors.LightText)
	logoShineStyle = lipgloss.NewStyle().Foreground(Colors.FocusOrange)
}

var (
	logoArt = []string{
		`  ██████╗ ███╗   ██╗ ██████╗███████╗`,
		` ██╔═══██╗████╗  ██║██╔════╝██╔════╝`,
		` ██║   ██║██╔██╗ ██║██║     █████╗  `,
		` ██║   ██║██║╚██╗██║██║     ██╔══╝  `,
		` ╚██████╔╝██║ ╚████║╚██████╗███████╗`,
		`  ╚═════╝ ╚═╝  ╚═══╝ ╚═════╝╚══════╝`,
	}
)

type logoShineStartMsg struct{}
type logoShineStepMsg struct{}

type Logo struct {
	lines    [][]rune
	shinePos int
	maxDiag  int
}

func NewLogo() *Logo {
	lines := make([][]rune, len(logoArt))
	maxWidth := 0
	for i, line := range logoArt {
		lines[i] = []rune(line)
		if len(lines[i]) > maxWidth {
			maxWidth = len(lines[i])
		}
	}

	return &Logo{
		lines:    lines,
		shinePos: -1,
		maxDiag:  maxWidth + len(logoArt),
	}
}

func (l *Logo) Init() tea.Cmd {
	return tea.Tick(logoShineDelay, func(time.Time) tea.Msg {
		return logoShineStartMsg{}
	})
}

func (l *Logo) Update(msg tea.Msg) tea.Cmd {
	switch msg.(type) {
	case logoShineStartMsg:
		l.shinePos = 0
		return l.shineTick()
	case logoShineStepMsg:
		l.shinePos += logoShineStep
		if l.shinePos > l.maxDiag+logoShineBand {
			l.shinePos = -1
			return tea.Tick(logoShineInterval, func(time.Time) tea.Msg {
				return logoShineStartMsg{}
			})
		}
		return l.shineTick()
	}
	return nil
}

func (l *Logo) View() string {
	var sb strings.Builder
	for i, line := range l.lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(l.renderLine(line, i))
	}
	return sb.String()
}

// Private

func (l *Logo) renderLine(line []rune, row int) string {
	if l.shinePos < 0 {
		return logoBaseStyle.Render(string(line))
	}

	shineStart := l.shinePos - row
	shineEnd := shineStart + logoShineBand

	lineLen := len(line)
	if shineStart >= lineLen || shineEnd <= 0 {
		return logoBaseStyle.Render(string(line))
	}

	shineStart = max(shineStart, 0)
	shineEnd = min(shineEnd, lineLen)

	var sb strings.Builder
	if shineStart > 0 {
		sb.WriteString(logoBaseStyle.Render(string(line[:shineStart])))
	}
	sb.WriteString(logoShineStyle.Render(string(line[shineStart:shineEnd])))
	if shineEnd < lineLen {
		sb.WriteString(logoBaseStyle.Render(string(line[shineEnd:])))
	}
	return sb.String()
}

func (l *Logo) shineTick() tea.Cmd {
	return tea.Tick(logoShineTickRate, func(time.Time) tea.Msg {
		return logoShineStepMsg{}
	})
}
