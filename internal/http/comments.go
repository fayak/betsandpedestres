package http

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"betsandpedestres/internal/http/middleware"
	"betsandpedestres/internal/notify"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type CommentCreateHandler struct {
	DB       *pgxpool.Pool
	Notifier notify.Notifier
	BaseURL  string
}

func (h *CommentCreateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)
	if uid == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	role, err := middleware.GetUserRole(ctx, h.DB, uid)
	if err != nil {
		slog.Error("comment.role", "err", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if role == middleware.RoleUnverified {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	betID := r.PathValue("id")
	if betID == "" {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	content := strings.TrimSpace(r.Form.Get("content"))
	if content == "" {
		http.Redirect(w, r, "/bets/"+betID+"#comments", http.StatusSeeOther)
		return
	}
	if len([]rune(content)) > 2000 {
		runes := []rune(content)
		content = string(runes[:2000])
	}

	parentID := strings.TrimSpace(r.Form.Get("parent_id"))
	if parentID != "" {
		var parentBet string
		if err := h.DB.QueryRow(ctx, `select bet_id::text from comments where id = $1::uuid`, parentID).Scan(&parentBet); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				parentID = ""
			} else {
				slog.Error("comment.parent", "err", err)
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
		} else if parentBet != betID {
			parentID = ""
		}
	}

	var commentID string
	if err := h.DB.QueryRow(ctx, `
		insert into comments (bet_id, user_id, content, parent_comment_id)
		values ($1::uuid, $2::uuid, $3, nullif($4,'')::uuid)
		returning id::text
	`, betID, uid, content, parentID).Scan(&commentID); err != nil {
		slog.Error("comment.insert", "err", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	if h.Notifier != nil {
		go h.notifyComment(ctx, betID, uid, commentID, content)
	}

	http.Redirect(w, r, "/bets/"+betID+"#comments", http.StatusSeeOther)
}

type CommentReactHandler struct {
	DB *pgxpool.Pool
}

func (h *CommentReactHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)
	if uid == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	direction := strings.ToLower(strings.TrimSpace(r.Form.Get("direction")))
	var value int
	switch direction {
	case "up":
		value = 1
	case "down":
		value = -1
	default:
		http.Error(w, "invalid reaction", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	role, err := middleware.GetUserRole(ctx, h.DB, uid)
	if err != nil {
		slog.Error("comment.react.role", "err", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if role == middleware.RoleUnverified {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	commentID := r.PathValue("id")
	if commentID == "" {
		http.NotFound(w, r)
		return
	}

	tx, err := h.DB.Begin(ctx)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	var betID string
	if err := tx.QueryRow(ctx, `select bet_id::text from comments where id = $1::uuid`, commentID).Scan(&betID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		slog.Error("comment.react.bet", "err", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	var prevValue int
	var prevExists bool
	err = tx.QueryRow(ctx, `select value from comment_reactions where comment_id = $1::uuid and user_id = $2::uuid`, commentID, uid).Scan(&prevValue)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("comment.react.prev", "err", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
	} else {
		prevExists = true
		if prevValue == value {
			// No change
			http.Redirect(w, r, redirectTarget(r, betID), http.StatusSeeOther)
			return
		}
	}

	deltaUp, deltaDown := 0, 0
	if prevExists {
		if prevValue == 1 {
			deltaUp--
		} else if prevValue == -1 {
			deltaDown--
		}
		_, err = tx.Exec(ctx, `update comment_reactions set value = $3 where comment_id = $1::uuid and user_id = $2::uuid`, commentID, uid, value)
		if err != nil {
			slog.Error("comment.react.update", "err", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
	} else {
		_, err = tx.Exec(ctx, `insert into comment_reactions (comment_id, user_id, value) values ($1::uuid, $2::uuid, $3)`, commentID, uid, value)
		if err != nil {
			slog.Error("comment.react.insert", "err", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
	}

	if value == 1 {
		deltaUp++
	} else if value == -1 {
		deltaDown++
	}

	if _, err := tx.Exec(ctx, `
		update comments
		set upvotes = upvotes + $2,
		    downvotes = downvotes + $3
		where id = $1::uuid
	`, commentID, deltaUp, deltaDown); err != nil {
		slog.Error("comment.react.update_counts", "err", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, redirectTarget(r, betID), http.StatusSeeOther)
}

func redirectTarget(r *http.Request, betID string) string {
	if ref := strings.TrimSpace(r.Header.Get("Referer")); ref != "" {
		return ref
	}
	return "/bets/" + betID + "#comments"
}

func (h *CommentCreateHandler) notifyComment(ctx context.Context, betID, userID, commentID, content string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var displayName, betTitle string
	if err := h.DB.QueryRow(ctx, `select display_name from users where id = $1::uuid`, userID).Scan(&displayName); err != nil {
		return
	}
	if err := h.DB.QueryRow(ctx, `select title from bets where id = $1::uuid`, betID).Scan(&betTitle); err != nil {
		return
	}

	link := betLink(h.BaseURL, betID)
	commentLink := link + "#comment-" + commentID

	truncated := content
	if len([]rune(truncated)) > 200 {
		runes := []rune(truncated)
		truncated = string(runes[:200]) + "…"
	}

	msg := notify.HTMLPrefix + fmt.Sprintf(
		"%s posted a new comment on <a href=\"%s\">%s</a>\n&gt; %s\n<a href=\"%s\">View comment</a>",
		html.EscapeString(displayName),
		html.EscapeString(link),
		html.EscapeString(betTitle),
		html.EscapeString(truncated),
		html.EscapeString(commentLink),
	)
	h.Notifier.NotifyGroup(ctx, msg)
	h.Notifier.NotifySubscribers(ctx, msg)
}
