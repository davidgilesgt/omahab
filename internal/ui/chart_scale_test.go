package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNiceMaxPercent(t *testing.T) {
	assert.Equal(t, 0.0, niceMax(UnitPercent, 0))
	assert.Equal(t, 100.0, niceMax(UnitPercent, 1))
	assert.Equal(t, 100.0, niceMax(UnitPercent, 100))
	assert.Equal(t, 200.0, niceMax(UnitPercent, 101))
	assert.Equal(t, 300.0, niceMax(UnitPercent, 250))
}

func TestNiceMaxBytes(t *testing.T) {
	const (
		KB = 1024.0
		MB = 1024 * KB
		GB = 1024 * MB
	)

	assert.Equal(t, 0.0, niceMax(UnitBytes, 0))
	assert.Equal(t, 1*MB, niceMax(UnitBytes, 500*KB))
	assert.Equal(t, 1*MB, niceMax(UnitBytes, 1*MB))
	assert.Equal(t, 10*MB, niceMax(UnitBytes, 5*MB))
	assert.Equal(t, 100*MB, niceMax(UnitBytes, 50*MB))
	assert.Equal(t, 1*GB, niceMax(UnitBytes, 500*MB))
	assert.Equal(t, 2*GB, niceMax(UnitBytes, 1.5*GB))
}

func TestNiceMaxCount(t *testing.T) {
	assert.Equal(t, 0.0, niceMax(UnitCount, 0))
	assert.Equal(t, 100.0, niceMax(UnitCount, 50))
	assert.Equal(t, 1_000.0, niceMax(UnitCount, 500))
	assert.Equal(t, 10_000.0, niceMax(UnitCount, 5_000))
	assert.Equal(t, 50_000.0, niceMax(UnitCount, 20_000))
	assert.Equal(t, 100_000.0, niceMax(UnitCount, 75_000))
	assert.Equal(t, 250_000.0, niceMax(UnitCount, 150_000))
	assert.Equal(t, 500_000.0, niceMax(UnitCount, 400_000))
	assert.Equal(t, 1_000_000.0, niceMax(UnitCount, 750_000))
	assert.Equal(t, 2_000_000.0, niceMax(UnitCount, 1_500_000))
}

func TestNewChartScale(t *testing.T) {
	s := NewChartScale(UnitPercent, 37)
	assert.Equal(t, 100.0, s.Max())

	s = NewChartScale(UnitBytes, 0)
	assert.Equal(t, 0.0, s.Max())
}
