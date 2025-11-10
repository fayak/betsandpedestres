package http

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"betsandpedestres/internal/auth"
	"betsandpedestres/internal/http/middleware"
	"betsandpedestres/internal/notify"
	"betsandpedestres/internal/web"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PasswordRecoveryHandler struct {
	DB       *pgxpool.Pool
	TPL      *web.Renderer
	Notifier notify.Notifier
}

type recoveryContent struct {
	Title  string
	Status string
}

func (h *PasswordRecoveryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.render(w, r, r.URL.Query().Get("status"))
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			h.render(w, r, "badform")
			return
		}
		action := strings.TrimSpace(r.Form.Get("action"))
		switch action {
		case "request":
			h.handleRequest(w, r)
		case "reset":
			h.handleReset(w, r)
		default:
			h.render(w, r, "badform")
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *PasswordRecoveryHandler) handleRequest(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(strings.ToLower(r.Form.Get("username")))
	if username == "" {
		h.render(w, r, "missing")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var (
		userID      string
		displayName string
		chatID      *int64
	)
	err := h.DB.QueryRow(ctx, `select id::text, display_name, telegram_chat_id from users where lower(username) = $1`, username).Scan(&userID, &displayName, &chatID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.render(w, r, "unknown")
		} else {
			slog.Error("recover.lookup", "err", err)
			h.render(w, r, "error")
		}
		return
	}
	if chatID == nil || *chatID == 0 {
		h.render(w, r, "notlinked")
		return
	}
	token := generateRecoveryToken()
	expires := time.Now().UTC().Add(10 * time.Minute)
	if _, err := h.DB.Exec(ctx, `
		insert into password_recoveries (user_id, token, expires_at)
		values ($1::uuid, $2, $3)
		on conflict (user_id) do update set token = $2, expires_at = $3
	`, userID, token, expires); err != nil {
		slog.Error("recover.upsert", "err", err)
		h.render(w, r, "error")
		return
	}

	msg := notify.HTMLPrefix + fmt.Sprintf(
		"Password recovery token for %s: <code>%s</code>\nValid for 10 minutes.",
		html.EscapeString(displayName),
		html.EscapeString(token),
	)
	h.Notifier.NotifyUser(ctx, userID, msg)
	h.render(w, r, "sent")
}

func (h *PasswordRecoveryHandler) handleReset(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(strings.ToLower(r.Form.Get("username")))
	tokenInput := strings.TrimSpace(strings.ToUpper(r.Form.Get("token")))
	newPass := strings.TrimSpace(r.Form.Get("new_password"))
	confirm := strings.TrimSpace(r.Form.Get("confirm_password"))
	if username == "" || tokenInput == "" || newPass == "" || confirm == "" {
		h.render(w, r, "missing")
		return
	}
	if newPass != confirm {
		h.render(w, r, "mismatch")
		return
	}
	if len([]rune(newPass)) < 6 {
		h.render(w, r, "weak")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var (
		storedToken string
		expires     time.Time
		userID      string
	)
	err := h.DB.QueryRow(ctx, `
		select pr.token, pr.expires_at, pr.user_id::text
		from password_recoveries pr
		join users u on u.id = pr.user_id
		where lower(u.username) = $1
	`, username).Scan(&storedToken, &expires, &userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.render(w, r, "invalid")
		} else {
			slog.Error("recover.fetch", "err", err)
			h.render(w, r, "error")
		}
		return
	}
	if time.Now().UTC().After(expires) {
		h.render(w, r, "expired")
		return
	}
	if strings.ToUpper(storedToken) != tokenInput {
		h.render(w, r, "invalid")
		return
	}

	hash, err := auth.HashPassword(newPass)
	if err != nil {
		h.render(w, r, "error")
		return
	}
	if _, err := h.DB.Exec(ctx, `update users set password_hash = $2 where id = $1::uuid`, userID, hash); err != nil {
		slog.Error("recover.update", "err", err)
		h.render(w, r, "error")
		return
	}
	_, _ = h.DB.Exec(ctx, `delete from password_recoveries where user_id = $1::uuid`, userID)

	token, err := auth.IssueToken(userID)
	if err != nil {
		h.render(w, r, "error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   false,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(72 * time.Hour),
	})
	http.Redirect(w, r, "/profile?pwd=recovered", http.StatusSeeOther)
}

func (h *PasswordRecoveryHandler) render(w http.ResponseWriter, r *http.Request, status string) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	uid := middleware.UserID(r)
	header, _ := loadHeader(ctx, h.DB, uid)
	content := recoveryContent{
		Title:  "Account recovery",
		Status: status,
	}
	page := web.Page[recoveryContent]{Header: header, Content: content}
	var buf bytes.Buffer
	if err := h.TPL.Render(&buf, "recover", page); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func generateRecoveryToken() string {
	const letters = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "RESETME"
	}
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}
