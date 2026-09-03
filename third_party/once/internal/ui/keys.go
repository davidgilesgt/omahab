package ui

import (
	"charm.land/bubbles/v2/key"
)

func NewKeyBinding(keys ...string) key.Binding {
	return key.NewBinding(key.WithKeys(keys...))
}

func WithHelp(b key.Binding, k, desc string) key.Binding {
	b.SetHelp(k, desc)
	return b
}
