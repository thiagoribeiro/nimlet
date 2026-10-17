package domain

import "time"

type UserType string

const (
	UserTypeHuman   UserType = "human"
	UserTypeAgent   UserType = "agent"
	UserTypeBot     UserType = "bot"
	UserTypeSystem  UserType = "system"
	UserTypeService UserType = "service"
)

type User struct {
	ID          string
	NamespaceID string
	Username    string
	DisplayName string
	Type        UserType
	Disabled    bool
	CreatedAt   time.Time
}
