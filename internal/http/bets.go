package http

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"betsandpedestres/internal/http/middleware"
	"betsandpedestres/internal/web"
	"github.com/jackc/pgx/v5/pgxpool"
)

type BetNewHandler struct {
	DB  *pgxpool.Pool
	TPL *web.Renderer
}

type betNewContent struct {
	Title string
}

func (h *BetNewHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)

	// Shared header
	header := web.HeaderData{}
	if uid != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		_ = h.DB.QueryRow(ctx, `
			select u.username, u.display_name, coalesce(b.balance,0)
			from users u
			left join user_balances b on b.user_id = u.id
			where u.id = $1
		`, uid).Scan(&header.Username, &header.DisplayName, &header.Balance)
		if header.Username != "" {
			header.LoggedIn = true
		}
	}

	page := web.Page[betNewContent]{
		Header:  header,
		Content: betNewContent{Title: "Create a new bet"},
	}

	var buf bytes.Buffer
	if err := h.TPL.Render(&buf, "bet_new", page); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

type BetCreateHandler struct {
	DB *pgxpool.Pool
}

func (h *BetCreateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented yet", http.StatusNotImplemented)
}

