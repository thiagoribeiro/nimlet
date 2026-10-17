package domain

import "time"

// Media is the metadata of an uploaded file.
type Media struct {
	ID          string
	NamespaceID string
	OwnerUserID string
	ContentType string
	CreatedAt   time.Time
}
