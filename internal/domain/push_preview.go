package domain

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// PreviewKind tells a client how to label a message push preview in its own language.
type PreviewKind string

const (
	PreviewText  PreviewKind = "text"  // text is the message body
	PreviewImage PreviewKind = "image" // text is the caption or filename, possibly empty
	PreviewFile  PreviewKind = "file"  // text is the caption or filename, possibly empty
	PreviewCard  PreviewKind = "card"  // text is the card's title or first markdown text
)

// MaxPreviewRunes bounds a push preview; the preview transits the push provider.
const MaxPreviewRunes = 120

// MessagePreview returns what a per-message push shows for e, or ok=false when e is
// not a message worth a push (reactions, plugin state, commands, agent status).
func MessagePreview(e *Event) (kind PreviewKind, text string, ok bool) {
	if e.AgentActivity() {
		return "", "", false
	}
	switch e.EventType {
	case "m.room.message":
		var c struct {
			Body string `json:"body"`
		}
		if json.Unmarshal(e.Content, &c) != nil || strings.TrimSpace(c.Body) == "" {
			return "", "", false
		}
		return PreviewText, shortPreview(c.Body), true
	case "m.room.attachment":
		var c struct {
			Caption  string `json:"caption"`
			Filename string `json:"filename"`
			Mimetype string `json:"mimetype"`
		}
		if json.Unmarshal(e.Content, &c) != nil {
			return "", "", false
		}
		kind = PreviewFile
		if strings.HasPrefix(c.Mimetype, "image/") {
			kind = PreviewImage
		}
		label := c.Caption
		if strings.TrimSpace(label) == "" {
			label = c.Filename
		}
		return kind, shortPreview(label), true
	case "m.ui.message":
		var c struct {
			Schema string `json:"schema"`
			Body   []struct {
				Type             string `json:"type"`
				Text             string `json:"text"`
				FallbackMarkdown string `json:"fallback_markdown"`
				View             struct {
					Title    string `json:"title"`
					Question string `json:"question"`
				} `json:"view"`
			} `json:"body"`
		}
		if json.Unmarshal(e.Content, &c) != nil || c.Schema != "brian.message" {
			return "", "", false
		}
		for _, b := range c.Body {
			for _, candidate := range []string{b.View.Title, b.View.Question, b.Text, b.FallbackMarkdown} {
				if strings.TrimSpace(candidate) != "" {
					return PreviewCard, shortPreview(candidate), true
				}
			}
		}
		return PreviewCard, "", true
	}
	return "", "", false
}

// shortPreview collapses whitespace and cuts at MaxPreviewRunes with an ellipsis.
func shortPreview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= MaxPreviewRunes {
		return s
	}
	runes := []rune(s)
	return strings.TrimRight(string(runes[:MaxPreviewRunes-1]), " ") + "…"
}
