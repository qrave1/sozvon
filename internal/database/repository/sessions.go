package repository

import (
	"context"
	"time"

	"github.com/qrave1/sozvon/internal/domain"
	"gorm.io/gorm"
)

type Sessions struct{ db *gorm.DB }

type sessionRow struct {
	ID         string    `gorm:"primaryKey;type:text"`
	UserID     string    `gorm:"index;type:text;not null"`
	TokenHash  string    `gorm:"uniqueIndex;type:text;not null"`
	UserAgent  string    `gorm:"type:text;not null"`
	CreatedAt  time.Time `gorm:"not null"`
	LastUsedAt time.Time `gorm:"not null"`
	ExpiresAt  time.Time `gorm:"index;not null"`
	User       userRow   `gorm:"foreignKey:UserID;references:ID;constraint:OnDelete:CASCADE"`
}

func (sessionRow) TableName() string { return "refresh_tokens" }

func NewSessions(db *gorm.DB) *Sessions { return &Sessions{db: db} }
func toSession(r sessionRow) domain.Session {
	return domain.Session{ID: r.ID, UserID: r.UserID, TokenHash: r.TokenHash, UserAgent: r.UserAgent, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt, ExpiresAt: r.ExpiresAt}
}

func (r *Sessions) Create(ctx context.Context, session domain.Session) error {
	ua := []rune(session.UserAgent)
	if len(ua) > 512 {
		session.UserAgent = string(ua[:512])
	}
	row := sessionRow{ID: session.ID, UserID: session.UserID, TokenHash: session.TokenHash, UserAgent: session.UserAgent, CreatedAt: session.CreatedAt.UTC(), LastUsedAt: session.LastUsedAt.UTC(), ExpiresAt: session.ExpiresAt.UTC()}
	return toErr(gorm.G[sessionRow](r.db).Create(ctx, &row))
}
func (r *Sessions) ByTokenHash(ctx context.Context, hash string, now time.Time) (domain.Session, error) {
	row, err := gorm.G[sessionRow](r.db).Where("token_hash = ? AND expires_at > ?", hash, now.UTC()).First(ctx)
	if err != nil {
		return domain.Session{}, toErr(err)
	}
	return toSession(row), nil
}
func (r *Sessions) Valid(ctx context.Context, id, userID string, now time.Time) (bool, error) {
	_, err := gorm.G[sessionRow](r.db).Where("id = ? AND user_id = ? AND expires_at > ?", id, userID, now.UTC()).First(ctx)
	if err == nil {
		return true, nil
	}
	if toErr(err) == ErrNotFound {
		return false, nil
	}
	return false, err
}
func (r *Sessions) Rotate(ctx context.Context, id, oldHash, newHash string, now time.Time) error {
	rows, err := gorm.G[sessionRow](r.db).Where("id = ? AND token_hash = ? AND expires_at > ?", id, oldHash, now.UTC()).Updates(ctx, sessionRow{TokenHash: newHash, LastUsedAt: now.UTC()})
	return affected(rows, err)
}
func (r *Sessions) Devices(ctx context.Context, userID string, now time.Time) ([]domain.Device, error) {
	rows, err := gorm.G[sessionRow](r.db).Select("id", "user_agent", "created_at", "last_used_at", "expires_at").Where("user_id = ? AND expires_at > ?", userID, now.UTC()).Order("created_at DESC").Find(ctx)
	if err != nil {
		return nil, err
	}
	devices := make([]domain.Device, 0, len(rows))
	for _, row := range rows {
		devices = append(devices, domain.Device{ID: row.ID, UserAgent: row.UserAgent, CreatedAt: row.CreatedAt, LastUsedAt: row.LastUsedAt, ExpiresAt: row.ExpiresAt})
	}
	return devices, nil
}

// Revoke removes one session owned by the user; existing calls are unaffected.
func (r *Sessions) Revoke(ctx context.Context, id, userID string) error {
	rows, err := gorm.G[sessionRow](r.db).Where("id = ? AND user_id = ?", id, userID).Delete(ctx)
	return affected(rows, err)
}
func (r *Sessions) RevokeOthers(ctx context.Context, userID, keepID string) error {
	_, err := gorm.G[sessionRow](r.db).Where("user_id = ? AND id != ?", userID, keepID).Delete(ctx)
	return err
}
func (r *Sessions) DeleteExpired(ctx context.Context, now time.Time) error {
	_, err := gorm.G[sessionRow](r.db).Where("expires_at <= ?", now.UTC()).Delete(ctx)
	return err
}
