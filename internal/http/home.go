package http

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"time"

	"betsandpedestres/internal/http/middleware"
	"betsandpedestres/internal/web"
	"github.com/jackc/pgx/v5/pgxpool"
)

type HomeHandler struct {
	DB  *pgxpool.Pool
	TPL *web.Renderer
}

func (h *HomeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)

	header := web.HeaderData{}
	if uid != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		err := h.DB.QueryRow(ctx, `
			select u.username, u.display_name, coalesce(b.balance,0)
			from users u
			left join user_balances b on b.user_id = u.id
			where u.id = $1
		`, uid).Scan(&header.Username, &header.DisplayName, &header.Balance)
		if err != nil {
			slog.Error("db error", "error", err)
		}
		if header.Username != "" {
			header.LoggedIn = true
		}
	}

	page := web.Page[struct{}]{Header: header}

	var buf bytes.Buffer
	if err := h.TPL.Render(&buf, "home", page); err != nil {
		slog.Error("template error", "error", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}
