package domain

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMessagePreview(t *testing.T) {
	long := strings.Repeat("ação ", 40)
	for _, tc := range []struct {
		name, eventType, content string
		kind                     PreviewKind
		text                     string
		ok                       bool
	}{
		{"text", "m.room.message", `{"msgtype":"m.text","body":"  oi\n\ntudo bem?  "}`, PreviewText, "oi tudo bem?", true},
		{"empty text", "m.room.message", `{"msgtype":"m.text","body":"   "}`, "", "", false},
		{"image with caption", "m.room.attachment", `{"mimetype":"image/jpeg","filename":"a.jpg","caption":"praia"}`, PreviewImage, "praia", true},
		{"file name", "m.room.attachment", `{"mimetype":"application/pdf","filename":"boleto.pdf"}`, PreviewFile, "boleto.pdf", true},
		{"card title", "m.ui.message", `{"schema":"brian.message","version":"1.0","body":[{"type":"plugin","plugin":"checklist","view":{"title":"Compras"}}]}`, PreviewCard, "Compras", true},
		{"card markdown", "m.ui.message", `{"schema":"brian.message","version":"1.0","body":[{"type":"markdown","text":"**Pronto**"}]}`, PreviewCard, "**Pronto**", true},
		{"agent status", "m.ui.message", `{"schema":"brian.message","version":"1.0","body":[{"type":"plugin","plugin":"agent_status","view":{"title":"Pensando"}}]}`, "", "", false},
		{"plugin state", "m.plugin.state.updated", `{"plugin":"checklist","state":{}}`, "", "", false},
		{"reaction", "m.room.reaction", `{"relates_to":"x","key":"👍"}`, "", "", false},
		{"command", "m.plugin.command", `{"plugin":"poll","command":"vote"}`, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, text, ok := MessagePreview(&Event{EventType: tc.eventType, Content: []byte(tc.content)})
			if kind != tc.kind || text != tc.text || ok != tc.ok {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", kind, text, ok, tc.kind, tc.text, tc.ok)
			}
		})
	}
	_, text, _ := MessagePreview(&Event{EventType: "m.room.message", Content: []byte(`{"body":"` + long + `"}`)})
	if utf8.RuneCountInString(text) != MaxPreviewRunes || !strings.HasSuffix(text, "…") {
		t.Fatalf("long preview %d runes: %q", utf8.RuneCountInString(text), text)
	}
}
