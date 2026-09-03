package ui

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/basecamp/once/internal/system"
)

func TestDashboardHeader_NarrowTerminalHidden(t *testing.T) {
	h := newTestHeader()

	assert.Equal(t, 0, h.Height(70))
	assert.Equal(t, "", h.View(70))
}

func TestDashboardHeader_WideTerminalShown(t *testing.T) {
	h := newTestHeader()

	assert.Equal(t, headerTotalHeight, h.Height(100))
	view := h.View(100)
	assert.NotEmpty(t, view)
	assert.Contains(t, view, "CPU")
	assert.Contains(t, view, "Memory")
	assert.Contains(t, view, "Disk")
}

func TestDashboardHeader_WithDiskData(t *testing.T) {
	s := system.NewScraper(system.ScraperSettings{
		BufferSize:   10,
		DiskFallback: "/",
	})
	s.Scrape(context.Background())
	h := NewDashboardHeader(s)

	view := h.View(120)
	assert.Contains(t, view, "% used")
	assert.Contains(t, view, "free")
}

func TestDashboardHeader_MemoryChartWithData(t *testing.T) {
	s := system.NewScraper(system.ScraperSettings{
		BufferSize:   10,
		DiskFallback: "/",
	})
	s.Scrape(context.Background())
	h := NewDashboardHeader(s)

	view := h.View(120)
	assert.Contains(t, view, "Memory")
}

func TestFormatDiskSize(t *testing.T) {
	assert.Equal(t, "500KB", formatDiskSize(500_000))
	assert.Equal(t, "50MB", formatDiskSize(50_000_000))
	assert.Equal(t, "500GB", formatDiskSize(500_000_000_000))
	assert.Equal(t, "1.0TB", formatDiskSize(1_000_000_000_000))
	assert.Equal(t, "2.5TB", formatDiskSize(2_500_000_000_000))
}

// Helpers

func newTestHeader() DashboardHeader {
	s := system.NewScraper(system.ScraperSettings{BufferSize: 10})
	return NewDashboardHeader(s)
}
