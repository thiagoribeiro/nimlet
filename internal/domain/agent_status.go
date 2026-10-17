package domain

import "encoding/json"

// AgentActivity is telemetry, not a new message notification. Delivery via sync
// and the appservice dispatcher remains identical to other events.
func (e *Event) AgentActivity() bool {
	var content struct {
		Plugin  string `json:"plugin"`
		Schema  string `json:"schema"`
		Version string `json:"version"`
		Body    []struct {
			Type   string `json:"type"`
			Plugin string `json:"plugin"`
		} `json:"body"`
	}
	if json.Unmarshal(e.Content, &content) != nil {
		return false
	}
	if e.EventType == "m.plugin.state.updated" || e.EventType == "m.plugin.instance.created" {
		return content.Plugin == "agent_status"
	}
	if e.EventType != "m.ui.message" || content.Schema != "brian.message" || content.Version != "1.0" || len(content.Body) == 0 {
		return false
	}
	for _, block := range content.Body {
		if block.Type != "plugin" || block.Plugin != "agent_status" {
			return false
		}
	}
	return true
}
