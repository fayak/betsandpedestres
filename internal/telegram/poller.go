package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Poller struct {
	db       *pgxpool.Pool
	botToken string
	client   *http.Client
}

type update struct {
	UpdateID int              `json:"update_id"`
	Message  *incomingMessage `json:"message"`
}

type incomingMessage struct {
	MessageID int    `json:"message_id"`
	Text      string `json:"text"`
	Chat      struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Date int64 `json:"date"`
}

type updatesResponse struct {
	OK     bool     `json:"ok"`
	Result []update `json:"result"`
}

func NewPoller(db *pgxpool.Pool, token string) *Poller {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	return &Poller{
		db:       db,
		botToken: strings.TrimSpace(token),
		client:   &http.Client{Timeout: 35 * time.Second},
	}
}

func (p *Poller) Run(ctx context.Context) {
	if p == nil {
		return
	}
	slog.Info("telegram.poller.start")
	defer slog.Info("telegram.poller.stop")
	var offset int
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		updates, err := p.fetchUpdates(ctx, offset)
		if err != nil {
			slog.Warn("telegram.poller.fetch", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}
		for _, upd := range updates {
			if upd.UpdateID >= offset {
				offset = upd.UpdateID + 1
			}
			p.handleUpdate(ctx, upd)
		}
	}
}

func (p *Poller) fetchUpdates(ctx context.Context, offset int) ([]update, error) {
	data := url.Values{}
	if offset > 0 {
		data.Set("offset", fmt.Sprintf("%d", offset))
	}
	data.Set("timeout", "30")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates", p.botToken), strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var res updatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	if !res.OK {
		return nil, fmt.Errorf("telegram api returned not ok")
	}
	return res.Result, nil
}

func (p *Poller) handleUpdate(ctx context.Context, upd update) {
	if upd.Message == nil || upd.Message.Text == "" {
		return
	}
	text := strings.TrimSpace(upd.Message.Text)
	lower := strings.ToLower(text)
	slog.Info("Telegram: received a message", "tg_message", lower)
	if strings.HasPrefix(lower, "/register") {
		p.handleRegister(ctx, upd.Message, text)
	}
}

func (p *Poller) handleRegister(ctx context.Context, msg *incomingMessage, original string) {
	parts := strings.Fields(original)
	if len(parts) != 2 {
		p.reply(msg.Chat.ID, "Usage: /register <your-user-id>")
		return
	}
	userID := parts[1]
	if _, err := uuid.Parse(userID); err != nil {
		p.reply(msg.Chat.ID, "That doesn't look like a valid user ID.")
		return
	}
	ctxDB, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var displayName string
	err := p.db.QueryRow(ctxDB, `
        update users
        set telegram_chat_id = $1
        where id = $2::uuid
        returning display_name
    `, msg.Chat.ID, userID).Scan(&displayName)
	if err != nil {
		p.reply(msg.Chat.ID, "We couldn't find that user ID. Double-check and try again.")
		return
	}
	p.reply(msg.Chat.ID, fmt.Sprintf("Thanks %s! Telegram alerts are now enabled.", displayName))
}

func (p *Poller) reply(chatID int64, message string) {
	if p == nil || p.botToken == "" {
		return
	}
	SendMessage(context.Background(), p.client, p.botToken, fmt.Sprintf("%d", chatID), message)
}
