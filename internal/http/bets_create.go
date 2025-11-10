package http

import (
	"bytes"
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
	"betsandpedestres/internal/web"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func (h *BetNewHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)

	header, role := loadHeader(r.Context(), h.DB, uid)
	if !header.LoggedIn || role == middleware.RoleUnverified {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
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
	DB       *pgxpool.Pool
	Notifier notify.Notifier
	BaseURL  string
}

var (
	errMissingTitle    = errors.New("title is required")
	errInvalidOptions  = errors.New("bet must have 2 to 10 distinct outcomes")
	errInvalidDeadline = errors.New("invalid deadline")
)

type betForm struct {
	Title       string
	Description string
	ExternalURL string
	Deadline    *time.Time
	Options     []string
}

func (h *BetCreateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)
	if uid == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	role, err := middleware.GetUserRole(ctx, h.DB, uid)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if role == middleware.RoleUnverified {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	form, err := parseBetForm(r)
	if err != nil {
		switch {
		case errors.Is(err, errMissingTitle),
			errors.Is(err, errInvalidOptions),
			errors.Is(err, errInvalidDeadline):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, "bad form", http.StatusBadRequest)
		}
		return
	}

	ctxCreate, cancelCreate := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancelCreate()

	betID, err := h.createBet(ctxCreate, uid, form)
	if err != nil {
		slog.Error("create bet error", "error", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	if h.Notifier != nil {
		link := betLink(h.BaseURL, betID)
		author := fetchDisplayName(ctx, h.DB, uid)
		message := formatNewBetGroupMessage(form, author, link)
		h.Notifier.NotifyGroup(r.Context(), message)
		h.Notifier.NotifyUser(r.Context(), uid, fmt.Sprintf("Your bet \"%s\" is live!\n%s", form.Title, link))
	}

	// Redirect to bet page
	http.Redirect(w, r, "/bets/"+betID, http.StatusSeeOther)
}

func parseBetForm(r *http.Request) (betForm, error) {
	form := betForm{
		Title:       strings.TrimSpace(r.Form.Get("title")),
		Description: strings.TrimSpace(r.Form.Get("description")),
		ExternalURL: strings.TrimSpace(r.Form.Get("external_url")),
	}
	if form.Title == "" {
		return betForm{}, errMissingTitle
	}

	opts, err := collectOptions(r.Form["option"])
	if err != nil {
		return betForm{}, err
	}
	form.Options = opts

	deadlineLocal := strings.TrimSpace(r.Form.Get("deadline_local"))
	deadlineUTC := strings.TrimSpace(r.Form.Get("deadline_utc"))
	tz := strings.TrimSpace(r.Form.Get("tz"))
	form.Deadline, err = parseDeadline(deadlineLocal, deadlineUTC, tz)
	if err != nil {
		return betForm{}, err
	}

	return form, nil
}

func collectOptions(raw []string) ([]string, error) {
	opts := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, o := range raw {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		key := strings.ToLower(o)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		opts = append(opts, o)
	}
	if len(opts) < 2 || len(opts) > 10 {
		return nil, errInvalidOptions
	}
	return opts, nil
}

func parseDeadline(localValue, fallbackUTC, tz string) (*time.Time, error) {
	if localValue == "" && fallbackUTC == "" {
		return nil, nil
	}
	if tz == "" {
		tz = "Europe/Paris"
	}
	if localValue != "" {
		if t, err := parseLocalDeadline(localValue, tz); err == nil {
			utc := t.In(time.UTC)
			return &utc, nil
		}
	}
	if fallbackUTC != "" {
		tm, err := time.Parse(time.RFC3339, fallbackUTC)
		if err == nil {
			utc := tm.In(time.UTC)
			return &utc, nil
		}
	}
	return nil, errInvalidDeadline
}

func parseLocalDeadline(value, tz string) (time.Time, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc, err = time.LoadLocation("Europe/Paris")
		if err != nil {
			return time.Time{}, err
		}
	}
	layouts := []string{
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, value, loc); err == nil {
			return t, nil
		}
		slog.Warn("Invalid deadline submitted", "deadline", value, "tz", tz, "error", err)
	}
	return time.Time{}, errInvalidDeadline
}

func (h *BetCreateHandler) createBet(ctx context.Context, uid string, form betForm) (string, error) {
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	betID, err := h.insertBet(ctx, tx, uid, form)
	if err != nil {
		return "", err
	}
	if err := h.insertOptions(ctx, tx, betID, form.Options); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return betID, nil
}

func fetchDisplayName(ctx context.Context, db *pgxpool.Pool, uid string) string {
	if db == nil || uid == "" {
		return "Anonymous"
	}
	var name string
	if err := db.QueryRow(ctx, `select coalesce(nullif(display_name,''), username) from users where id = $1::uuid`, uid).Scan(&name); err != nil {
		return "Anonymous"
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "Anonymous"
	}
	return name
}

func formatNewBetGroupMessage(form betForm, authorName, link string) string {
	safeTitle := html.EscapeString(form.Title)
	safeAuthor := html.EscapeString(authorName)
	safeLink := html.EscapeString(link)
	var builder strings.Builder
	builder.WriteString(notify.HTMLPrefix)
	builder.WriteString(fmt.Sprintf("New bet ! <strong><a href=\"%s\">%s</a></strong> ! 👀\n", safeLink, safeTitle))
	builder.WriteString(fmt.Sprintf("Submitted by %s.\n", safeAuthor))
	desc := truncateRunes(form.Description, 200)
	if desc != "" {
		builder.WriteString("\n")
		builder.WriteString(html.EscapeString(desc))
		builder.WriteString("\n")
	}
	builder.WriteString("\nOptions:\n")
	for _, opt := range form.Options {
		builder.WriteString("- ")
		builder.WriteString(html.EscapeString(opt))
		builder.WriteString("\n")
	}
	if form.Deadline != nil {
		builder.WriteString("\n 📅 deadline: ")
		builder.WriteString(form.Deadline.UTC().Format("02 Jan 2006 15:04 MST"))
		builder.WriteString("\n")
	}
	builder.WriteString("\nGo vote ! 🗳️")
	return builder.String()
}

func truncateRunes(s string, max int) string {
	if max <= 0 || s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func (h *BetCreateHandler) insertBet(ctx context.Context, tx pgx.Tx, uid string, form betForm) (string, error) {
	var betID string
	err := tx.QueryRow(ctx, `
		insert into bets (creator_user_id, title, description, external_url, deadline)
		values ($1, $2, $3, nullif($4,''), $5)
		returning id::text
	`, uid, form.Title, nullIfEmpty(form.Description), form.ExternalURL, form.Deadline).Scan(&betID)
	return betID, err
}

func (h *BetCreateHandler) insertOptions(ctx context.Context, tx pgx.Tx, betID string, opts []string) error {
	for i, label := range opts {
		if _, err := tx.Exec(ctx, `
			insert into bet_options (bet_id, label, position)
			values ($1, $2, $3)
		`, betID, label, i+1); err != nil {
			return err
		}
	}
	return nil
}
