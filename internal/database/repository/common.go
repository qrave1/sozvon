// Package repository contains GORM Generics operations for users and refresh sessions.
package repository

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

var ErrNotFound = errors.New("repository: not found")
var ErrConflict = errors.New("repository: conflict")
var ErrStale = errors.New("repository: stale value")

func toErr(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

func affected(rows int, err error) error {
	if err != nil {
		return toErr(err)
	}
	if rows == 0 {
		return ErrStale
	}
	return nil
}

// AutoMigrate runs during application startup. It is idempotent and retains data.
func AutoMigrate(db *gorm.DB) error { return db.AutoMigrate(&userRow{}, &sessionRow{}) }
