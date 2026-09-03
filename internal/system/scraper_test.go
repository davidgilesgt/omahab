package system

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchReturnsNewestFirst(t *testing.T) {
	s := newTestScraper(10)

	for i := range 5 {
		s.pushSample(Sample{CPUPercent: float64(i)})
	}

	samples := s.Fetch(5)
	require.Len(t, samples, 5)
	assert.Equal(t, 4.0, samples[0].CPUPercent)
	assert.Equal(t, 0.0, samples[4].CPUPercent)
}

func TestFetchReturnsAvailableWhenLessThanRequested(t *testing.T) {
	s := newTestScraper(10)
	s.pushSample(Sample{CPUPercent: 42})

	samples := s.Fetch(5)
	require.Len(t, samples, 1)
	assert.Equal(t, 42.0, samples[0].CPUPercent)
}

func TestRingBufferWraps(t *testing.T) {
	s := newTestScraper(3)

	for i := range 5 {
		s.pushSample(Sample{CPUPercent: float64(i)})
	}

	samples := s.Fetch(3)
	require.Len(t, samples, 3)
	assert.Equal(t, 4.0, samples[0].CPUPercent)
	assert.Equal(t, 3.0, samples[1].CPUPercent)
	assert.Equal(t, 2.0, samples[2].CPUPercent)
}

func TestFetchEmpty(t *testing.T) {
	s := newTestScraper(10)
	samples := s.Fetch(5)
	assert.Empty(t, samples)
}

func TestNumCPUs(t *testing.T) {
	s := newTestScraper(1)
	assert.Greater(t, s.NumCPUs(), 0)
}

func TestScrapeCollectsSample(t *testing.T) {
	s := NewScraper(ScraperSettings{
		BufferSize:   10,
		DiskFallback: "/",
	})

	s.Scrape(context.Background())
	samples := s.Fetch(1)
	require.Len(t, samples, 1)
	assert.Greater(t, samples[0].NumCPUs, 0)
}

func TestScrapeCollectsMemory(t *testing.T) {
	s := NewScraper(ScraperSettings{
		BufferSize:   10,
		DiskFallback: "/",
	})

	s.Scrape(context.Background())
	samples := s.Fetch(1)
	require.Len(t, samples, 1)
	assert.Greater(t, samples[0].MemTotal, uint64(0))
	assert.Greater(t, samples[0].MemUsed, uint64(0))
	assert.Greater(t, s.MemTotal(), uint64(0))
}

func TestDiskPathFallback(t *testing.T) {
	s := &Scraper{
		settings: ScraperSettings{
			DiskPath:     "/nonexistent/path/for/test",
			DiskFallback: "/",
		},
	}
	assert.Equal(t, "/", s.diskPath())
}

func TestDiskPathPrimary(t *testing.T) {
	s := &Scraper{
		settings: ScraperSettings{
			DiskPath:     "/",
			DiskFallback: "/tmp",
		},
	}
	assert.Equal(t, "/", s.diskPath())
}

// Helpers

func newTestScraper(bufSize int) *Scraper {
	return NewScraper(ScraperSettings{
		BufferSize:   bufSize,
		DiskFallback: "/",
	})
}

func (s *Scraper) pushSample(sample Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.samples[s.head] = sample
	s.head = (s.head + 1) % len(s.samples)
	if s.count < len(s.samples) {
		s.count++
	}
}
