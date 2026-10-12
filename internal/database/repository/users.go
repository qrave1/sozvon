package repository

import (
	"context"
	"time"

	"github.com/qrave1/sozvon/internal/domain"
	"gorm.io/gorm"
)

type Users struct{ db *gorm.DB }

type userRow struct {
	ID           string    `gorm:"primaryKey;type:text"`
	Login        string    `gorm:"uniqueIndex;type:text;not null;collate:BINARY"`
	DisplayName  string    `gorm:"type:text;not null"`
	PasswordHash string    `gorm:"type:text;not null"`
	CreatedAt    time.Time `gorm:"not null"`
	UpdatedAt    time.Time `gorm:"not null"`
}

func (userRow) TableName() string { return "users" }

func NewUsers(db *gorm.DB) *Users { return &Users{db: db} }
func toUser(r userRow) domain.User {
	return domain.User{ID: r.ID, Login: r.Login, DisplayName: r.DisplayName, PasswordHash: r.PasswordHash, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func (r *Users) Create(ctx context.Context, user domain.User) error {
	row := userRow{ID: user.ID, Login: user.Login, DisplayName: user.DisplayName, PasswordHash: user.PasswordHash, CreatedAt: user.CreatedAt.UTC(), UpdatedAt: user.UpdatedAt.UTC()}
	return toErr(gorm.G[userRow](r.db).Create(ctx, &row))
}
func (r *Users) ByID(ctx context.Context, id string) (domain.User, error) {
	row, err := gorm.G[userRow](r.db).Where("id = ?", id).First(ctx)
	if err != nil {
		return domain.User{}, toErr(err)
	}
	return toUser(row), nil
}
func (r *Users) ByLogin(ctx context.Context, login string) (domain.User, error) {
	row, err := gorm.G[userRow](r.db).Where("login = ? COLLATE BINARY", login).First(ctx)
	if err != nil {
		return domain.User{}, toErr(err)
	}
	return toUser(row), nil
}
func (r *Users) SetDisplayName(ctx context.Context, id, name string, now time.Time) error {
	rows, err := gorm.G[userRow](r.db).Where("id = ?", id).Updates(ctx, userRow{DisplayName: name, UpdatedAt: now.UTC()})
	return affected(rows, err)
}

// ChangePasswordAndKeepSession updates the password and revokes other sessions atomically.
func (r *Users) ChangePasswordAndKeepSession(ctx context.Context, userID, previousHash, nextHash, keepSessionID string, now time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		rows, err := gorm.G[userRow](tx).Where("id = ? AND password_hash = ?", userID, previousHash).Updates(ctx, userRow{PasswordHash: nextHash, UpdatedAt: now.UTC()})
		if err := affected(rows, err); err != nil {
			return err
		}
		_, err = gorm.G[sessionRow](tx).Where("user_id = ? AND id != ?", userID, keepSessionID).Delete(ctx)
		return err
	})
}
