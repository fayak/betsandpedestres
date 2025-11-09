package http

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"betsandpedestres/internal/auth"
	"betsandpedestres/internal/notify"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type AccountRegisterHandler struct {
	DB       *pgxpool.Pool
	Notifier notify.Notifier
}

func (h *AccountRegisterHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/?signup=error", http.StatusSeeOther)
		return
	}
	username := strings.TrimSpace(r.Form.Get("username"))
	displayName := strings.TrimSpace(r.Form.Get("display_name"))
	password := r.Form.Get("password")
	if username == "" || displayName == "" || password == "" {
		http.Redirect(w, r, "/?signup=missing", http.StatusSeeOther)
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		http.Redirect(w, r, "/?signup=error", http.StatusSeeOther)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	_, err = h.DB.Exec(ctx, `
		insert into users (username, display_name, password_hash, role)
		values ($1, $2, $3, 'unverified')
	`, username, displayName, hash)
	if err != nil {
		if pgErr, ok := err.(*pgconn.PgError); ok && pgErr.Code == "23505" {
			http.Redirect(w, r, "/?signup=exists", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/?signup=error", http.StatusSeeOther)
		return
	}

	if h.Notifier != nil {
		h.Notifier.NotifyAdmins(ctx, fmt.Sprintf("New account requested: %s (%s)", username, displayName))
	}

	http.Redirect(w, r, "/?signup=ok", http.StatusSeeOther)
}
