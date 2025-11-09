package http

import (
	"context"
	"time"

	"betsandpedestres/internal/web"
	"github.com/jackc/pgx/v5/pgxpool"
)

func loadHeader(ctx context.Context, db *pgxpool.Pool, uid string) (web.HeaderData, string) {
	header := web.HeaderData{}
	if uid == "" {
		return header, ""
	}
	ctxHead, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var role string
	err := db.QueryRow(ctxHead, `
			select u.username, u.display_name, coalesce(b.balance,0), u.role
			from users u
			left join user_balances b on b.user_id = u.id
			where u.id = $1
		`, uid).Scan(&header.Username, &header.DisplayName, &header.Balance, &role)
	if err == nil && header.Username != "" {
		header.LoggedIn = true
	}
	return header, role
}
