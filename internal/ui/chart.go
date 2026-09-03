package ui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

const (
	ChartHistoryLength      = 200
	ChartUpdateInterval     = 2 * time.Second
	UserStatsUpdateInterval = 30 * time.Second
	ChartSlidingWindow      = int(time.Minute / ChartUpdateInterval)
)

type UnitType int

const (
	UnitCount   UnitType = iota // 1K, 1M (requests, errors)
	UnitPercent                 // 50.0%
	UnitBytes                   // 128 MiB, 1.5 GiB
)

func (u UnitType) Format(value float64) string {
	switch u {
	case UnitPercent:
		return fmt.Sprintf("%.0f%%", value)
	case UnitBytes:
		const (
			KiB = 1024
			MiB = KiB * 1024
			GiB = MiB * 1024
		)
		switch {
		case value >= GiB:
			return fmt.Sprintf("%.1fG", value/GiB)
		case value >= MiB:
			return fmt.Sprintf("%.0fM", value/MiB)
		case value >= KiB:
			return fmt.Sprintf("%.0fK", value/KiB)
		default:
			return fmt.Sprintf("%.0fB", value)
		}
	default: // UnitCount
		if value >= 1_000_000 {
			return fmt.Sprintf("%.1fM", value/1_000_000)
		}
		if value >= 1_000 {
			return fmt.Sprintf("%.1fK", value/1_000)
		}
		return fmt.Sprintf("%.0f", value)
	}
}

// braille bit patterns for left and right columns
// Each column has 4 dots, allowing 2 data points per character.
// Left column dots (bottom to top): 7, 3, 2, 1
// Right column dots (bottom to top): 8, 6, 5, 4
const brailleBase rune = 0x2800

var (
	leftDots  = [4]rune{0x40, 0x04, 0x02, 0x01} // dots 7, 3, 2, 1
	rightDots = [4]rune{0x80, 0x20, 0x10, 0x08} // dots 8, 6, 5, 4
)

// Chart renders a braille histogram with a vertical color gradient.
// The constructor takes static properties (title, unit) and View takes
// per-render values (data, width, height).
type Chart struct {
	title string
	unit  UnitType
}

func NewChart(title string, unit UnitType) Chart {
	return Chart{title: title, unit: unit}
}

// View renders the chart as a string with a rounded border.
// The title is embedded in the top border line. Inner rows contain
// the braille chart with max-value and "0" labels on the first and last rows.
func (c Chart) View(data []float64, width, height int, scale ChartScale) string {
	if width <= 0 || height <= 2 {
		return ""
	}

	chartRows := max(height-2, 1) // minus top and bottom border
	innerWidth := width - 2       // minus left and right border chars

	// Ensure data fills the inner width (each chart char = 2 data points)
	dataPoints := innerWidth * 2
	padded := make([]float64, dataPoints)
	srcStart := max(0, len(data)-dataPoints)
	dstStart := max(0, dataPoints-len(data))
	copy(padded[dstStart:], data[srcStart:])

	maxVal := scale.Max()
	displayMax := maxVal
	if maxVal == 0 {
		maxVal = 1
	}

	// Format labels and calculate label width
	maxLabel := c.unit.Format(displayMax)
	labelWidth := max(lipgloss.Width(maxLabel), 1)
	chartWidth := innerWidth - labelWidth - 1 // -1 for space between label and chart

	if chartWidth <= 0 {
		return ""
	}

	// Each character row represents 4 vertical dots
	dotsHeight := chartRows * 4

	// Calculate the height in dots for each data point
	heights := make([]int, len(padded))
	for i, v := range padded {
		heights[i] = int((v / maxVal) * float64(dotsHeight))
		if v > 0 && heights[i] == 0 {
			heights[i] = 1
		}
	}

	var lines []string
	lines = append(lines, boxTop(c.title, innerWidth))

	// Build the chart row by row, from top to bottom
	dataOffset := max(0, len(heights)-chartWidth*2)

	labelStyle := lipgloss.NewStyle().Foreground(Colors.Border).Width(labelWidth).Align(lipgloss.Left)
	left := boxSide()
	right := boxSide()

	for row := range chartRows {
		var sb strings.Builder
		rowBottomDot := (chartRows - 1 - row) * 4
		rowTopDot := rowBottomDot + 4

		for col := range chartWidth {
			dataIdxLeft := dataOffset + col*2
			dataIdxRight := dataOffset + col*2 + 1

			char := brailleBase

			if dataIdxLeft < len(heights) {
				char |= brailleColumn(heights[dataIdxLeft], rowBottomDot, rowTopDot, leftDots)
			}

			if dataIdxRight < len(heights) {
				char |= brailleColumn(heights[dataIdxRight], rowBottomDot, rowTopDot, rightDots)
			}

			sb.WriteRune(char)
		}

		var label string
		switch row {
		case 0:
			label = labelStyle.Render(maxLabel)
		case chartRows - 1:
			label = labelStyle.Render("0")
		default:
			label = labelStyle.Render("")
		}

		// Gradient from green (bottom) to orange (top)
		t := float64(chartRows-1-row) / float64(max(chartRows-1, 1))
		rowColor := Colors.Gradient(t)
		chartRow := lipgloss.NewStyle().Foreground(rowColor).Render(sb.String())
		lines = append(lines, left+label+" "+chartRow+right)
	}

	lines = append(lines, boxBottom(innerWidth))

	return strings.Join(lines, "\n")
}

// brailleColumn returns the braille bits for a single column based on height
func brailleColumn(h, rowBottom, rowTop int, dots [4]rune) rune {
	if h <= rowBottom {
		return 0
	}

	var bits rune
	dotsToFill := min(h-rowBottom, 4)
	for i := range dotsToFill {
		bits |= dots[i]
	}
	return bits
}

// SlidingSum computes the sum of each point and the preceding window-1 points.
// Missing values before the start of data are treated as zero.
// Returns same length as input.
func SlidingSum(data []float64, window int) []float64 {
	if len(data) == 0 || window <= 0 {
		return data
	}

	result := make([]float64, len(data))
	for i := range data {
		var sum float64
		start := max(0, i-window+1)
		for j := start; j <= i; j++ {
			sum += data[j]
		}
		result[i] = sum
	}
	return result
}

// Helpers

func barColor(pct float64) HealthState {
	switch {
	case pct > 85:
		return healthError
	case pct >= 60:
		return healthWarning
	default:
		return healthNormal
	}
}

func maxValue(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	m := data[0]
	for _, v := range data[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

func peakValue(data []float64, window int) float64 {
	if len(data) == 0 || window <= 0 {
		return 0
	}
	start := max(0, len(data)-window)
	return maxValue(data[start:])
}
