package docker

import (
	"context"
	"strings"
	"time"

	"github.com/moby/moby/client"
)

type EventWatcher struct {
	client    *client.Client
	namespace string
}

func NewEventWatcher(c *client.Client, namespace string) *EventWatcher {
	return &EventWatcher{
		client:    c,
		namespace: namespace,
	}
}

// Watch returns a channel that signals when a container in this namespace
// changes state. The channel is closed when the context is cancelled.
func (w *EventWatcher) Watch(ctx context.Context) <-chan struct{} {
	out := make(chan struct{})

	go func() {
		defer close(out)
		w.watchLoop(ctx, out)
	}()

	return out
}

// Private

func (w *EventWatcher) watchLoop(ctx context.Context, out chan<- struct{}) {
	for {
		w.streamEvents(ctx, out)

		select {
		case <-ctx.Done():
			return
		case <-time.After(streamRetryDelay): // Pause before retrying
		}
	}
}

func (w *EventWatcher) streamEvents(ctx context.Context, out chan<- struct{}) {
	filters := client.Filters{}.
		Add("type", "container").
		Add("event", "start", "stop", "die", "restart")

	events := w.client.Events(ctx, client.EventsListOptions{Filters: filters})

	prefix := w.namespace + "-"

	for {
		select {
		case <-ctx.Done():
			return
		case <-events.Err:
			return
		case event := <-events.Messages:
			name := event.Actor.Attributes["name"]
			if strings.HasPrefix(name, prefix) {
				select {
				case out <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}
