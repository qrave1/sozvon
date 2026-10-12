// Package domain contains account and session data without persistence details.
package domain

import "time"

type User struct {
	ID           string
	Login        string
	DisplayName  string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Session struct {
	ID         string
	UserID     string
	TokenHash  string
	UserAgent  string
	CreatedAt  time.Time
	LastUsedAt time.Time
	ExpiresAt  time.Time
}

// Device contains the public fields of an active session.
type Device struct {
	ID         string
	UserAgent  string
	CreatedAt  time.Time
	LastUsedAt time.Time
	ExpiresAt  time.Time
}
