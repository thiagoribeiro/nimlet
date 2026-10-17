package http

import (
	"context"

	"github.com/thiagoribeiro/nimlet/internal/port/in"
)

type actorKey struct{}

// Actor holds the authenticated principal extracted by the auth middleware.
// NamespaceID comes from the session (or the AppService registration) only.
type Actor struct {
	SessionID   string
	NamespaceID string
	UserID      string
	DeviceID    string
}

// principal is the actor as the use cases see it.
func (a Actor) principal() in.Actor {
	return in.Actor{NamespaceID: a.NamespaceID, UserID: a.UserID, DeviceID: a.DeviceID}
}

func withActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

func actorFrom(ctx context.Context) Actor {
	a, _ := ctx.Value(actorKey{}).(Actor)
	return a
}
