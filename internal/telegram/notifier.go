package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"betsandpedestres/internal/notify"
	"github.com/jackc/pgx/v5/pgxpool"
)

const apiURL = "https://api.telegram.org/bot%s/sendMessage"

type Notifier struct {
	db          *pgxpool.Pool
	botToken    string
	groupChatID string
}

func New(db *pgxpool.Pool, botToken, groupChatID string) notify.Notifier {
	if botToken == "" {
		return notify.Noop{}
	}
	return &Notifier{
		db:          db,
		botToken:    botToken,
		groupChatID: strings.TrimSpace(groupChatID),
	}
}

func (n *Notifier) NotifyAdmins(ctx context.Context, msg string) {
	if n == nil || n.botToken == "" {
		return
	}
	rows, err := n.db.Query(ctx, `select telegram_chat_id::text from users where role = 'admin' and telegram_chat_id is not null`)
	if err != nil {
		slog.Warn("telegram.admin_query_failed", "err", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var chatID string
		if err := rows.Scan(&chatID); err != nil {
			slog.Warn("telegram.scan_chat_id", "err", err)
			continue
		}
		sendMessage(ctx, nil, n.botToken, chatID, msg)
	}
}

func (n *Notifier) NotifyGroup(ctx context.Context, msg string) {
	if n == nil || n.botToken == "" || n.groupChatID == "" {
		return
	}
	sendMessage(ctx, nil, n.botToken, n.groupChatID, msg)
}

func (n *Notifier) NotifyUser(ctx context.Context, userID string, msg string) {
	if n == nil || n.botToken == "" || userID == "" {
		return
	}
	var chatID int64
	if err := n.db.QueryRow(ctx, `select telegram_chat_id from users where id = $1::uuid`, userID).Scan(&chatID); err != nil {
		return
	}
	if chatID == 0 {
		return
	}
	sendMessage(ctx, nil, n.botToken, fmt.Sprintf("%d", chatID), msg)
}

var defaultHTTPClient = &http.Client{
	Timeout: 5 * time.Second,
}

// SendMessage sends a Telegram message using the provided HTTP client (or default if nil).
func SendMessage(ctx context.Context, client *http.Client, token, chatID, msg string) {
	sendMessage(ctx, client, token, chatID, msg)
}

func sendMessage(ctx context.Context, client *http.Client, token, chatID, msg string) {
	if token == "" || chatID == "" {
		return
	}
	if client == nil {
		client = defaultHTTPClient
	}
	payload := map[string]string{
		"chat_id": chatID,
		"text":    msg,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("telegram.marshal", "err", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf(apiURL, token), bytes.NewReader(body))
	if err != nil {
		slog.Warn("telegram.request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("telegram.send", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		slog.Warn("telegram.send.status", "status", resp.Status)
	}
}
