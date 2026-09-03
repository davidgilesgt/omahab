package ui

import "math"

type ChartScale struct {
	max float64
}

func NewChartScale(unit UnitType, rawMax float64) ChartScale {
	return ChartScale{max: niceMax(unit, rawMax)}
}

func (s ChartScale) Max() float64 {
	return s.max
}

type DashboardScales struct {
	CPU, Memory ChartScale // fixed scales from host hardware
	Traffic     ChartScale // shared across panels
}

// Helpers

func niceMax(unit UnitType, raw float64) float64 {
	if raw == 0 {
		return 0
	}

	switch unit {
	case UnitPercent:
		return math.Ceil(raw/100) * 100

	case UnitBytes:
		const (
			MiB = 1024 * 1024
			GiB = 1024 * MiB
		)
		steps := []float64{1 * MiB, 10 * MiB, 100 * MiB, 1 * GiB}
		for _, step := range steps {
			if raw <= step {
				return step
			}
		}
		return math.Ceil(raw/GiB) * GiB

	default: // UnitCount
		steps := []float64{100, 1_000, 10_000, 50_000, 100_000, 250_000, 500_000, 1_000_000}
		for _, step := range steps {
			if raw <= step {
				return step
			}
		}
		return math.Ceil(raw/1_000_000) * 1_000_000
	}
}
