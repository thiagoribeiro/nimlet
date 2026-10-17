package domain

import "time"

type Device struct {
	ID          string
	UserID      string
	DisplayName string
	Disabled    bool
	CreatedAt   time.Time
}

// DevicePushToken pairs a device with its registered FCM push token and owning user —
// the shape push delivery needs to fan a room event out to every device of every
// recipient, without pulling the whole Device record along.
type DevicePushToken struct {
	DeviceID  string
	UserID    string
	PushToken string
}
