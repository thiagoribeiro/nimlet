package domain

import (
	"encoding/json"
	"testing"
)

func TestAgentActivityOnlySuppressesStatusTelemetry(t *testing.T) {
	tests := []struct {
		kind, body string
		silent     bool
	}{
		{"m.plugin.state.updated", `{"plugin":"agent_status"}`, true},
		{"m.plugin.instance.created", `{"plugin":"agent_status"}`, true},
		{"m.ui.message", `{"schema":"brian.message","version":"1.0","body":[{"type":"plugin","plugin":"agent_status"}]}`, true},
		{"m.room.message", `{"body":"hello","plugin":"agent_status"}`, false},
		{"m.plugin.state.updated", `{"plugin":"workflow"}`, false},
		{"m.ui.message", `{"schema":"brian.message","version":"1.0","body":[{"type":"plugin","plugin":"agent_status"},{"type":"markdown","text":"answer"}]}`, false},
		{"m.ui.message", `{"schema":"brian.message","version":"1.0","body":[]}`, false},
		{"m.plugin.state.updated", `invalid`, false},
	}
	for _, tt := range tests {
		e := &Event{EventType: tt.kind, Content: json.RawMessage(tt.body)}
		if e.AgentActivity() != tt.silent {
			t.Errorf("%s %s", tt.kind, tt.body)
		}
	}
}
