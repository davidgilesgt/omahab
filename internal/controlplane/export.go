package controlplane

import (
	"github.com/omahab/omahab/internal/companion"
	"github.com/omahab/omahab/internal/events"
)

// EventsForTest returns the underlying events service for integration tests.
func (b *Backend) EventsForTest() *events.Service { return b.events }

// EnvironmentsForTest returns the companion environments service.
func (b *Backend) EnvironmentsForTest() *companion.Service { return b.environments }
