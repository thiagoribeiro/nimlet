// Package fcm implements push.Sender using FCM HTTP v1, authenticated directly
// with a Firebase service-account key — no Expo push relay in the delivery path,
// matching this project's self-hosted posture (see internal/push.Sender).
package fcm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/thiagoribeiro/nimlet/internal/push"
)

const (
	messagingScope = "https://www.googleapis.com/auth/firebase.messaging"
	defaultBaseURL = "https://fcm.googleapis.com"
	sendTimeout    = 10 * time.Second
)

type Sender struct {
	projectID   string
	tokenSource oauth2.TokenSource
	httpClient  *http.Client
	baseURL     string // overridable in tests; real FCM endpoint otherwise
}

// NewSender builds an FCM HTTP v1 push.Sender from a Firebase service-account key
// file. The target project ID is read from the credentials file itself (its
// "project_id" field), so callers only need the file path.
func NewSender(ctx context.Context, credentialsFile string) (*Sender, error) {
	raw, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("fcm: read credentials: %w", err)
	}

	// The type is pinned so a file of another credential type (for example an
	// external account pointing at arbitrary URLs) is rejected.
	creds, err := google.CredentialsFromJSONWithType(ctx, raw, google.ServiceAccount, messagingScope)
	if err != nil {
		return nil, fmt.Errorf("fcm: parse credentials: %w", err)
	}
	if creds.ProjectID == "" {
		return nil, fmt.Errorf("fcm: credentials file has no project_id")
	}

	return &Sender{
		projectID:   creds.ProjectID,
		tokenSource: creds.TokenSource,
		httpClient:  &http.Client{Timeout: sendTimeout},
		baseURL:     defaultBaseURL,
	}, nil
}

type sendRequest struct {
	Message fcmMessage `json:"message"`
}

type fcmMessage struct {
	Token   string            `json:"token"`
	Data    map[string]string `json:"data"`
	Android *androidConfig    `json:"android,omitempty"`
}

type androidConfig struct {
	Priority string `json:"priority"`
}

// Send delivers digest to token as an FCM data-only message — no "notification"
// field, deliberately: the client decides how (and whether) to render it, and no
// message content ever leaves Postgres (see push.Digest's doc comment).
func (s *Sender) Send(ctx context.Context, token string, digest push.Digest) error {
	roomsJSON, err := json.Marshal(digest.Rooms)
	if err != nil {
		return fmt.Errorf("fcm: encode digest rooms: %w", err)
	}

	return s.post(ctx, fcmMessage{
		Token: token,
		Data: map[string]string{
			"type":        "digest",
			"rooms":       string(roomsJSON),
			"total_count": strconv.Itoa(digest.TotalCount),
		},
	})
}

// SendMessage delivers one message as a data-only FCM message with high Android
// priority, so a device in Doze shows it promptly. The client renders it; the
// preview is the only message content that leaves the server (namespaces in
// message mode).
func (s *Sender) SendMessage(ctx context.Context, token string, msg push.MessagePush) error {
	return s.post(ctx, fcmMessage{
		Token: token,
		Data: map[string]string{
			"type":        "message",
			"room_id":     msg.RoomID,
			"room_name":   msg.RoomName,
			"event_id":    msg.EventID,
			"sender_id":   msg.SenderID,
			"sender_name": msg.SenderName,
			"kind":        string(msg.Kind),
			"preview":     msg.Preview,
		},
		Android: &androidConfig{Priority: "HIGH"},
	})
}

func (s *Sender) post(ctx context.Context, message fcmMessage) error {
	body, err := json.Marshal(sendRequest{Message: message})
	if err != nil {
		return fmt.Errorf("fcm: encode request: %w", err)
	}

	authToken, err := s.tokenSource.Token()
	if err != nil {
		return fmt.Errorf("fcm: get oauth token: %w", err)
	}

	url := fmt.Sprintf("%s/v1/projects/%s/messages:send", s.baseURL, s.projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("fcm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	authToken.SetAuthHeader(req)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fcm: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("fcm: send rejected: status=%d body=%s", resp.StatusCode, respBody)
	}
	return nil
}
