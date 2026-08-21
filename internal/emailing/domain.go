package emailing

import (
	"github.com/omahab/omahab/internal/domain"
)

// ToDomain converts a QuarantinedOrReceived view to domain.EmailMessage for
// compatibility with shared domain types. All content remains untrusted.
func (m QuarantinedOrReceived) ToDomain() domain.EmailMessage {
	return domain.EmailMessage{
		ID:             domain.ID(m.ID),
		EnvelopeFrom:   m.EnvelopeFrom,
		HeaderFrom:     m.HeaderFrom,
		Recipient:      m.Recipient,
		Subject:        m.Subject,
		Authentication: m.Authentication,
		Status:         m.Status,
		ReceivedAt:     m.ReceivedAt,
	}
}

// DomainMessages converts a slice of views to domain types.
func DomainMessages(in []QuarantinedOrReceived) []domain.EmailMessage {
	out := make([]domain.EmailMessage, 0, len(in))
	for _, m := range in {
		out = append(out, m.ToDomain())
	}
	return out
}
