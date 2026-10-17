package domain

import "strings"

// AppService is an external service registered to receive events from Nimlet
// and publish events back into the timeline.
type AppService struct {
	ID               string
	Endpoint         string   // base URL for outbound delivery (POST {Endpoint}/events)
	HSToken          string   // token Nimlet sends to the AS (Authorization: Bearer)
	ASToken          string   // token the AS sends to Nimlet when publishing events
	NamespaceSlug    string   // namespace the AS lives in (YAML `namespace`, default "default")
	NamespaceID      string   // resolved at startup from NamespaceSlug
	SenderUsername   string   // username of the user provisioned for this AS
	SenderUserID     string   // resolved at startup from SenderUsername
	SenderDeviceID   string   // device provisioned for this AS at startup
	InterestedEvents []string // event type patterns; "*" suffix is a glob
}

// IsInterestedIn reports whether this AppService wants to receive events of the given type.
// Patterns support a trailing "*" wildcard: "brian.*" matches "brian.block.action".
func (as *AppService) IsInterestedIn(eventType string) bool {
	for _, pattern := range as.InterestedEvents {
		if pattern == "*" {
			return true
		}
		if strings.HasSuffix(pattern, "*") {
			prefix := strings.TrimSuffix(pattern, "*")
			if strings.HasPrefix(eventType, prefix) {
				return true
			}
		} else if pattern == eventType {
			return true
		}
	}
	return false
}
