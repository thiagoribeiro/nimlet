package usecase_test

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	"github.com/thiagoribeiro/nimlet/internal/usecase"
	"github.com/thiagoribeiro/nimlet/testkit"
	"testing"
)

func TestRoomAgentStatusIsStoredAndDispatchedWithoutPush(t *testing.T) {
	for _, tc := range []struct {
		kind, body string
		pushes     int
	}{
		{"m.ui.message", `{"schema":"brian.message","version":"1.0","body":[{"type":"plugin","plugin":"agent_status"}]}`, 0},
		{"m.plugin.state.updated", `{"plugin":"agent_status","view":{"status":"running"}}`, 0},
		{"m.room.message", `{"body":"Resposta normal"}`, 1},
		{"m.plugin.state.updated", `{"plugin":"workflow","view":{"status":"running"}}`, 1},
	} {
		t.Run(tc.kind+tc.body, func(t *testing.T) {
			pushes := &testkit.MockPushDispatcher{}
			dispatch := &testkit.MockAppServiceDispatcher{}
			stored := false
			svc := usecase.NewRoomService(&testkit.MockRoomStore{}, &testkit.MockMembershipStore{
				GetFn: func(_ context.Context, _, id string) (*domain.Membership, error) {
					return joinedMembership("room", id, domain.RoleMember), nil
				},
			}, &testkit.MockEventStore{CreateFn: func(_ context.Context, e *domain.Event, _ bool) (*domain.Event, []string, error) {
				stored = true
				return e, []string{"sender", "reader"}, nil
			}},
				&testkit.MockEventBus{}, &testkit.MockTransactor{}, dispatch, pushes)
			_, err := svc.SendEvent(context.Background(), in.SendEventCmd{RoomID: "room", EventType: tc.kind, TxnID: "txn", Content: json.RawMessage(tc.body)}, "sender", "device")
			require.NoError(t, err)
			require.True(t, stored)
			require.Len(t, dispatch.Dispatched, 1)
			require.Len(t, pushes.Dispatched, tc.pushes)
		})
	}
}
