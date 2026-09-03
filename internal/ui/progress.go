package ui

import (
	"image/color"
	"math/rand/v2"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type Progress struct {
	width   int
	color   color.Color
	percent int

	pattern []rune
}

type ProgressTickMsg struct{}

func NewProgress(width int, clr color.Color) Progress {
	return Progress{
		width:   width,
		color:   clr,
		percent: -1,
		pattern: generateBraillePattern(width),
	}
}

func (p Progress) Init() tea.Cmd {
	return p.tick()
}

func (p Progress) Update(msg tea.Msg) (Progress, tea.Cmd) {
	switch msg.(type) {
	case ProgressTickMsg:
		p.pattern = generateBraillePattern(p.width)
		return p, p.tick()
	}
	return p, nil
}

func (p Progress) View() string {
	if p.width <= 0 {
		return ""
	}

	style := lipgloss.NewStyle().Foreground(p.color)

	if p.percent >= 0 && p.percent < 100 {
		return style.Render(string(p.renderBar()))
	}

	return style.Render(string(p.pattern))
}

func (p Progress) SetPercent(pct int) Progress {
	p.percent = pct
	return p
}

func (p Progress) SetWidth(w int) Progress {
	p.width = w
	p.pattern = generateBraillePattern(w)
	return p
}

// Private

func (p Progress) tick() tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg {
		return ProgressTickMsg{}
	})
}

func (p Progress) renderBar() []rune {
	resolution := p.width * 2
	filled := resolution * p.percent / 100
	fullChars := filled / 2
	halfChar := filled%2 == 1

	bar := make([]rune, p.width)
	for i := range bar {
		switch {
		case i < fullChars:
			bar[i] = '⣿' // U+28FF — all 8 dots
		case i == fullChars && halfChar:
			bar[i] = '⡇' // U+2847 — left column only
		default:
			bar[i] = ' '
		}
	}
	return bar
}

// Helpers

func generateBraillePattern(width int) []rune {
	pattern := make([]rune, width)
	for i := range pattern {
		// Braille patterns: U+2800 to U+28FF (256 patterns)
		pattern[i] = rune(0x2800 + rand.IntN(256))
	}
	return pattern
}
