package ui

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/basecamp/once/internal/system"
)

const (
	headerChartHeight = 6
	headerBorderLines = 2
	headerTotalHeight = headerChartHeight + headerBorderLines
	headerMinWidth    = 80
)

type DashboardHeader struct {
	scraper  *system.Scraper
	cpuChart Chart
	memChart Chart
}

func NewDashboardHeader(scraper *system.Scraper) DashboardHeader {
	cpuTitle := fmt.Sprintf("CPU (%d cores)", scraper.NumCPUs())

	memTitle := "Memory"
	if total := scraper.MemTotal(); total > 0 {
		memTitle = fmt.Sprintf("Memory (%s)", UnitBytes.Format(float64(total)))
	}

	return DashboardHeader{
		scraper:  scraper,
		cpuChart: NewChart(cpuTitle, UnitPercent),
		memChart: NewChart(memTitle, UnitBytes),
	}
}

func (h DashboardHeader) Height(width int) int {
	if width < headerMinWidth {
		return 0
	}
	return headerTotalHeight
}

func (h DashboardHeader) View(width int) string {
	if width < headerMinWidth {
		return ""
	}

	if total := h.scraper.MemTotal(); total > 0 {
		h.memChart = NewChart(fmt.Sprintf("Memory (%s)", UnitBytes.Format(float64(total))), UnitBytes)
	}

	gap := 1
	widths := distributeWidths(width-2*gap, 3) // 2 gaps between 3 panels

	cpuView := h.renderCPUChart(widths[0])
	memView := h.renderMemChart(widths[1])
	diskView := h.renderDiskGauge(widths[2])

	spacer := strings.Repeat(" ", gap)
	return lipgloss.JoinHorizontal(lipgloss.Top, cpuView, spacer, memView, spacer, diskView)
}

// Private

func (h DashboardHeader) renderCPUChart(width int) string {
	samples := h.scraper.Fetch(ChartHistoryLength)
	data := make([]float64, len(samples))
	for i, s := range samples {
		data[i] = s.CPUPercent
	}
	slices.Reverse(data)

	scaleMax := float64(h.scraper.NumCPUs()) * 100
	scale := ChartScale{max: scaleMax}

	return h.cpuChart.View(data, width, headerTotalHeight, scale)
}

func (h DashboardHeader) renderMemChart(width int) string {
	samples := h.scraper.Fetch(ChartHistoryLength)
	data := make([]float64, len(samples))
	for i, s := range samples {
		data[i] = float64(s.MemUsed)
	}
	slices.Reverse(data)

	scaleMax := float64(h.scraper.MemTotal())
	scale := ChartScale{max: scaleMax}

	return h.memChart.View(data, width, headerTotalHeight, scale)
}

func (h DashboardHeader) renderDiskGauge(width int) string {
	samples := h.scraper.Fetch(1)

	innerWidth := width - 2 // left + right border

	topLine := boxTop("Disk", innerWidth) + "\n"
	bottomLine := boxBottom(innerWidth)

	left := boxSide()
	right := boxSide()

	contentRows := headerChartHeight

	if len(samples) == 0 || samples[0].DiskErr != nil || samples[0].DiskTotal == 0 {
		emptyLines := make([]string, contentRows)
		for i := range emptyLines {
			padding := strings.Repeat(" ", innerWidth)
			emptyLines[i] = left + padding + right
		}
		return topLine + strings.Join(emptyLines, "\n") + "\n" + bottomLine
	}

	sample := samples[0]
	pct := float64(sample.DiskUsed) / float64(sample.DiskTotal) * 100

	barWidth := max(innerWidth-4, 0) // left indent(2) + right margin(2)
	bar := "  " + renderBar(pct, 0, 100, barColor(pct).Color(), barWidth)
	pctLine := formatValueLine(fmt.Sprintf("  %.0f%% used", pct), formatDiskSize(sample.DiskTotal)+" ", innerWidth)
	detailLine := fmt.Sprintf("  %s used, %s free", formatDiskSize(sample.DiskUsed), formatDiskSize(sample.DiskFree))

	lines := make([]string, contentRows)
	for i := range lines {
		var content string
		switch i {
		case 1:
			content = padOrTruncate(bar, innerWidth)
		case 2:
			content = pctLine
		case 4:
			content = padOrTruncate(detailLine, innerWidth)
		default:
			content = strings.Repeat(" ", innerWidth)
		}
		lines[i] = left + content + right
	}

	return topLine + strings.Join(lines, "\n") + "\n" + bottomLine
}

// Helpers

func formatDiskSize(bytes uint64) string {
	const (
		KB = 1000
		MB = KB * 1000
		GB = MB * 1000
		TB = GB * 1000
	)

	switch {
	case bytes >= TB:
		return fmt.Sprintf("%.1fTB", float64(bytes)/float64(TB))
	case bytes >= GB:
		return fmt.Sprintf("%.0fGB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.0fMB", float64(bytes)/float64(MB))
	default:
		return fmt.Sprintf("%.0fKB", float64(bytes)/float64(KB))
	}
}
