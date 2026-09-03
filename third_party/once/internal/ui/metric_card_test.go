package ui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMetricCardHealth(t *testing.T) {
	card := NewMetricCard("CPU", []float64{50}, ChartScale{max: 100}, UnitPercent, "", 60, 85)
	assert.Equal(t, healthNormal, card.Health())

	card = NewMetricCard("CPU", []float64{70}, ChartScale{max: 100}, UnitPercent, "", 60, 85)
	assert.Equal(t, healthWarning, card.Health())

	card = NewMetricCard("CPU", []float64{90}, ChartScale{max: 100}, UnitPercent, "", 60, 85)
	assert.Equal(t, healthError, card.Health())
}

func TestMetricCardHealthZeroScale(t *testing.T) {
	card := NewMetricCard("CPU", []float64{0}, ChartScale{max: 0}, UnitPercent, "", 60, 85)
	assert.Equal(t, healthNormal, card.Health())
}

func TestMetricCardViewContainsBorders(t *testing.T) {
	card := NewMetricCard("CPU", []float64{50}, ChartScale{max: 100}, UnitPercent, "", 60, 85)
	view := card.View(20)

	assert.Contains(t, view, "╭─CPU")
	assert.Contains(t, view, "╰")
	assert.Contains(t, view, "│")
}

func TestMetricCardViewContainsValue(t *testing.T) {
	card := NewMetricCard("CPU", []float64{50}, ChartScale{max: 100}, UnitPercent, "", 60, 85)
	view := card.View(20)

	assert.Contains(t, view, "50%")
}

func TestMetricCardViewContainsPeak(t *testing.T) {
	card := NewMetricCard("CPU", []float64{30, 80, 50}, ChartScale{max: 100}, UnitPercent, "", 60, 85)
	view := card.View(20)

	assert.Contains(t, view, "peak:")
	assert.Contains(t, view, "80%")
}

func TestMetricCardViewHasCorrectLineCount(t *testing.T) {
	card := NewMetricCard("CPU", []float64{50}, ChartScale{max: 100}, UnitPercent, "", 60, 85)
	view := card.View(20)

	lines := strings.Split(view, "\n")
	// Top border + 3 content rows + bottom border = 5
	assert.Equal(t, 5, len(lines))
}

func TestMetricCardViewWithLimitLabel(t *testing.T) {
	card := NewMetricCard("CPU", []float64{50}, ChartScale{max: 100}, UnitPercent, "200%", 60, 85)
	view := card.View(30)

	assert.Contains(t, view, "200%")
}

func TestTrafficCardViewContainsErrors(t *testing.T) {
	card := NewTrafficCard([]float64{100}, []float64{10}, ChartScale{max: 200}, 10, 3, 5)
	view := card.View(25)

	assert.Contains(t, view, "Traffic")
	assert.Contains(t, view, "/min")
	assert.Contains(t, view, "errors")
}

func TestTrafficCardZeroErrors(t *testing.T) {
	card := NewTrafficCard([]float64{100}, []float64{0}, ChartScale{max: 200}, 0, 3, 5)
	view := card.View(25)

	assert.Contains(t, view, "0% errors")
}

func TestHealthStateColor(t *testing.T) {
	// Just verify each state returns a non-nil color
	assert.NotNil(t, healthNormal.Color())
	assert.NotNil(t, healthWarning.Color())
	assert.NotNil(t, healthError.Color())
}

func TestMetricThresholdsHealth(t *testing.T) {
	thresholds := MetricThresholds{Warning: 60, Error: 85}

	assert.Equal(t, healthNormal, thresholds.Health(0))
	assert.Equal(t, healthNormal, thresholds.Health(59.9))
	assert.Equal(t, healthWarning, thresholds.Health(60))
	assert.Equal(t, healthWarning, thresholds.Health(84.9))
	assert.Equal(t, healthError, thresholds.Health(85))
	assert.Equal(t, healthError, thresholds.Health(100))
}
