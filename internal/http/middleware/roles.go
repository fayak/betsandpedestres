package middleware

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	RoleUnverified = "unverified"
	RoleUser       = "user"
	RoleModerator  = "moderator"
	RoleAdmin      = "admin"
)

func IsModerator(ctx context.Context, db *pgxpool.Pool, userID string) (bool, error) {
	var roleID string
	err := db.QueryRow(ctx, `select role from users where id = $1`, userID).Scan(&roleID)
	if err != nil {
		return false, err
	}
	return roleID == RoleModerator || roleID == RoleAdmin, nil
}

func GetUserRole(ctx context.Context, db *pgxpool.Pool, userID string) (string, error) {
	var roleID string
	err := db.QueryRow(ctx, `select role from users where id = $1`, userID).Scan(&roleID)
	return roleID, err
}
