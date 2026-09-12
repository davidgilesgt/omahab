package controlplane

import (
	"github.com/omahab/omahab/internal/companion"
	"github.com/omahab/omahab/internal/events"
)

// EventsForTest returns the underlying events service for integration tests.
func (b *Backend) EventsForTest() *events.Service { return b.events }

// Environments returns the companion environments service for production wiring
// (e.g. api.Config.Environments so deviceAuth validates device tokens).
func (b *Backend) Environments() *companion.Service { return b.environments }

 // EnvironmentsForTest returns the companion environments service.
 func (b *Backend) EnvironmentsForTest() *companion.Service { return b.environments }
