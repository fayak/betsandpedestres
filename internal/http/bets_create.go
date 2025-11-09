package http

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"betsandpedestres/internal/http/middleware"
	"betsandpedestres/internal/web"
	"github.com/jackc/pgx/v5/pgxpool"
)

func (h *BetNewHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)

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
	uid := middleware.UserID(r)
	if uid == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	title := strings.TrimSpace(r.Form.Get("title"))
	desc := strings.TrimSpace(r.Form.Get("description"))
	extURL := strings.TrimSpace(r.Form.Get("external_url"))
	deadlineUTC := strings.TrimSpace(r.Form.Get("deadline_utc"))
	// tz := r.Form.Get("tz") // received for reference; not persisted for now

	// Collect options (2–10, unique case-insensitive)
	rawOpts := r.Form["option"]
	opts := make([]string, 0, len(rawOpts))
	seen := map[string]struct{}{}
	for _, o := range rawOpts {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		key := strings.ToLower(o)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		opts = append(opts, o)
	}

	if title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	if len(opts) < 2 || len(opts) > 10 {
		http.Error(w, "bet must have 2 to 10 distinct outcomes", http.StatusBadRequest)
		return
	}

	var dl *time.Time
	if deadlineUTC != "" {
		tm, err := time.Parse(time.RFC3339, deadlineUTC)
		if err != nil {
			http.Error(w, "invalid deadline", http.StatusBadRequest)
			return
		}
		dl = &tm
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	tx, err := h.DB.Begin(ctx)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		slog.Error("db error", "error", err)
		return
	}
	defer tx.Rollback(ctx)

	var betID string
	err = tx.QueryRow(ctx, `
		insert into bets (creator_user_id, title, description, external_url, deadline)
		values ($1, $2, $3, nullif($4,''), $5)
		returning id::text
	`, uid, title, nullIfEmpty(desc), extURL, dl).Scan(&betID)
	if err != nil {
		http.Error(w, "insert bet error", http.StatusInternalServerError)
		return
	}

	// Insert options with positions 1..N
	for i, label := range opts {
		if _, err := tx.Exec(ctx, `
			insert into bet_options (bet_id, label, position)
			values ($1, $2, $3)
		`, betID, label, i+1); err != nil {
			http.Error(w, "insert options error", http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "commit error", http.StatusInternalServerError)
		return
	}

	// Redirect to bet page
	http.Redirect(w, r, "/bets/"+betID, http.StatusSeeOther)
}
