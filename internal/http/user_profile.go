package http

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"betsandpedestres/internal/auth"
	"betsandpedestres/internal/http/middleware"
	"betsandpedestres/internal/notify"
	"betsandpedestres/internal/web"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type UserProfileHandler struct {
	DB       *pgxpool.Pool
	TPL      *web.Renderer
	Notifier notify.Notifier
}

type profileUserInfo struct {
	ID             string
	Username       string
	DisplayName    string
	Role           string
	JoinedAt       time.Time
	TelegramChatID *int64
	TelegramNotify bool
}

type profileWallet struct {
	Balance int64
	Escrow  int64
}

type profileBet struct {
	ID        string
	Title     string
	CreatedAt time.Time
	Deadline  *time.Time
	Stakes    int64
}

type profileWager struct {
	BetID    string
	BetTitle string
	Amount   int64
	Deadline *time.Time
}

type profileTransaction struct {
	ID        string
	CreatedAt time.Time
	Reason    string
	Note      *string
	BetTitle  *string
	Delta     int64
}

type profileUserOption struct {
	Username    string
	DisplayName string
}

type profileContent struct {
	Title                string
	Target               profileUserInfo
	Wallet               profileWallet
	ActiveBets           []profileBet
	ActiveWagers         []profileWager
	Transactions         []profileTransaction
	ViewingOther         bool
	ShowUserPicker       bool
	UserOptions          []profileUserOption
	CanEditRoles         bool
	RoleUpdateStatus     string
	ShowTelegram         bool
	PasswordUpdateStatus string
	DisplayUpdateStatus  string
	NotifyUpdateStatus   string
	TransferStatus       string
}

func (h *UserProfileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)
	if uid == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	header, role := loadHeader(r.Context(), h.DB, uid)
	if !header.LoggedIn {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	targetUsername := header.Username
	pathUsername := r.PathValue("username")
	if pathUsername != "" {
		if role == middleware.RoleUnverified {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		targetUsername = pathUsername
	}

	if r.Method == http.MethodPost {
		if pathUsername == "" {
			if err := r.ParseForm(); err != nil {
				http.Redirect(w, r, "/profile?pwd=error", http.StatusSeeOther)
				return
			}
			switch strings.TrimSpace(r.Form.Get("action")) {
			case "password":
				h.handlePasswordChange(w, r, uid)
			case "display":
				h.handleDisplayChange(w, r, uid)
			case "notify":
				h.handleNotifyToggle(w, r, uid)
			case "transfer":
				h.handleTransfer(w, r, uid)
			default:
				http.Redirect(w, r, "/profile?pwd=error", http.StatusSeeOther)
			}
			return
		}
		if role != middleware.RoleAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		target := r.PathValue("username")
		if target == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		newRole := r.Form.Get("role")
		if !isValidRole(newRole) {
			http.Error(w, "bad role", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		targetDisplay, err := h.updateUserRole(ctx, uid, target, newRole)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		if targetDisplay != "" {
			msg := fmt.Sprintf("Admin %s set role for %s to %s", header.DisplayName, targetDisplay, newRole)
			h.Notifier.NotifyAdmins(ctx, msg)
		}
		http.Redirect(w, r, "/profile/"+target+"?role=updated", http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if targetUsername == "" {
		if err := h.DB.QueryRow(ctx, `select username from users where id = $1::uuid`, uid).Scan(&targetUsername); err != nil {
			http.Error(w, "could not load user", http.StatusInternalServerError)
			return
		}
	}

	targetUser, err := h.fetchUserInfo(ctx, targetUsername)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.NotFound(w, r)
		} else {
			http.Error(w, "db error", http.StatusInternalServerError)
		}
		return
	}

	wallet := h.fetchWallet(ctx, targetUser.ID)
	activeBets, err := h.fetchActiveBets(ctx, targetUser.ID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	activeWagers, err := h.fetchActiveWagers(ctx, targetUser.ID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	transactions, err := h.fetchTransactions(ctx, targetUser.ID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	var userOptions []profileUserOption
	showPicker := role != middleware.RoleUnverified
	if showPicker {
		userOptions, _ = h.fetchUserOptions(ctx)
	}

	content := profileContent{
		Title:                "Profile of " + targetUser.DisplayName,
		Target:               targetUser,
		Wallet:               wallet,
		ActiveBets:           activeBets,
		ActiveWagers:         activeWagers,
		Transactions:         transactions,
		ViewingOther:         targetUsername != header.Username,
		ShowUserPicker:       showPicker,
		UserOptions:          userOptions,
		RoleUpdateStatus:     r.URL.Query().Get("role"),
		CanEditRoles:         role == middleware.RoleAdmin,
		ShowTelegram:         targetUsername == header.Username,
		PasswordUpdateStatus: r.URL.Query().Get("pwd"),
		DisplayUpdateStatus:  r.URL.Query().Get("display"),
		NotifyUpdateStatus:   r.URL.Query().Get("notify"),
		TransferStatus:       r.URL.Query().Get("transfer"),
	}

	page := web.Page[profileContent]{Header: header, Content: content}

	var buf bytes.Buffer
	if err := h.TPL.Render(&buf, "user_profile", page); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func (h *UserProfileHandler) fetchUserInfo(ctx context.Context, username string) (profileUserInfo, error) {
	var info profileUserInfo
	err := h.DB.QueryRow(ctx, `
		select id::text, username, display_name, role, created_at, telegram_chat_id, telegram_notify
		from users
		where username = $1
	`, username).Scan(&info.ID, &info.Username, &info.DisplayName, &info.Role, &info.JoinedAt, &info.TelegramChatID, &info.TelegramNotify)
	return info, err
}

func (h *UserProfileHandler) fetchWallet(ctx context.Context, userID string) profileWallet {
	var wallet profileWallet
	_ = h.DB.QueryRow(ctx, `
		select coalesce(balance,0)::bigint
		from user_balances
		where user_id = $1::uuid
	`, userID).Scan(&wallet.Balance)

	_ = h.DB.QueryRow(ctx, `
		select coalesce(sum(w.amount),0)::bigint
		from wagers w
		join bets b on b.id = w.bet_id
		where w.user_id = $1::uuid and b.status = 'open'
	`, userID).Scan(&wallet.Escrow)

	return wallet
}

func (h *UserProfileHandler) fetchActiveBets(ctx context.Context, userID string) ([]profileBet, error) {
	rows, err := h.DB.Query(ctx, `
		select
			b.id::text,
			b.title,
			b.created_at,
			b.deadline,
			coalesce(sum(w.amount),0)::bigint as stakes
		from bets b
		left join wagers w on w.bet_id = b.id
		where b.creator_user_id = $1::uuid and b.status = 'open'
		group by b.id
		order by b.created_at desc
		limit 20
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []profileBet
	for rows.Next() {
		var b profileBet
		if err := rows.Scan(&b.ID, &b.Title, &b.CreatedAt, &b.Deadline, &b.Stakes); err != nil {
			return nil, err
		}
		list = append(list, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

func (h *UserProfileHandler) fetchActiveWagers(ctx context.Context, userID string) ([]profileWager, error) {
	rows, err := h.DB.Query(ctx, `
		select
			b.id::text,
			b.title,
			coalesce(sum(w.amount),0)::bigint as amt,
			b.deadline
		from wagers w
		join bets b on b.id = w.bet_id
		where w.user_id = $1::uuid and b.status = 'open'
		group by b.id
		order by b.deadline asc nulls last, b.title asc
		limit 20
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []profileWager
	for rows.Next() {
		var wrow profileWager
		if err := rows.Scan(&wrow.BetID, &wrow.BetTitle, &wrow.Amount, &wrow.Deadline); err != nil {
			return nil, err
		}
		list = append(list, wrow)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

func (h *UserProfileHandler) fetchTransactions(ctx context.Context, userID string) ([]profileTransaction, error) {
	rows, err := h.DB.Query(ctx, `
		select
			t.id::text,
			t.created_at,
			t.reason,
			b.title,
			t.note,
			le.delta
		from ledger_entries le
		join accounts a on a.id = le.account_id
		join transactions t on t.id = le.tx_id
		left join bets b on b.id = t.bet_id
		where a.user_id = $1::uuid
		order by t.created_at desc, t.id desc
		limit 20
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []profileTransaction
	for rows.Next() {
		var trow profileTransaction
		if err := rows.Scan(&trow.ID, &trow.CreatedAt, &trow.Reason, &trow.BetTitle, &trow.Note, &trow.Delta); err != nil {
			return nil, err
		}
		list = append(list, trow)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

func (h *UserProfileHandler) handlePasswordChange(w http.ResponseWriter, r *http.Request, uid string) {
	current := r.Form.Get("current_password")
	newPass := strings.TrimSpace(r.Form.Get("new_password"))
	confirm := strings.TrimSpace(r.Form.Get("confirm_password"))

	if current == "" || newPass == "" || confirm == "" {
		http.Redirect(w, r, "/profile?pwd=missing", http.StatusSeeOther)
		return
	}
	if newPass != confirm {
		http.Redirect(w, r, "/profile?pwd=mismatch", http.StatusSeeOther)
		return
	}
	if len([]rune(newPass)) < 6 {
		http.Redirect(w, r, "/profile?pwd=weak", http.StatusSeeOther)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var currentHash string
	if err := h.DB.QueryRow(ctx, `select password_hash from users where id = $1::uuid`, uid).Scan(&currentHash); err != nil {
		http.Redirect(w, r, "/profile?pwd=error", http.StatusSeeOther)
		return
	}
	if !auth.CheckPassword(current, currentHash) {
		http.Redirect(w, r, "/profile?pwd=invalid", http.StatusSeeOther)
		return
	}

	newHash, err := auth.HashPassword(newPass)
	if err != nil {
		http.Redirect(w, r, "/profile?pwd=error", http.StatusSeeOther)
		return
	}
	if _, err := h.DB.Exec(ctx, `update users set password_hash = $2 where id = $1::uuid`, uid, newHash); err != nil {
		http.Redirect(w, r, "/profile?pwd=error", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/profile?pwd=updated", http.StatusSeeOther)
}

func (h *UserProfileHandler) handleDisplayChange(w http.ResponseWriter, r *http.Request, uid string) {
	newName := strings.TrimSpace(r.Form.Get("display_name"))
	if newName == "" {
		http.Redirect(w, r, "/profile?display=missing", http.StatusSeeOther)
		return
	}
	if len([]rune(newName)) > 64 {
		runes := []rune(newName)
		newName = string(runes[:64])
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if _, err := h.DB.Exec(ctx, `update users set display_name = $2 where id = $1::uuid`, uid, newName); err != nil {
		http.Redirect(w, r, "/profile?display=error", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/profile?display=updated", http.StatusSeeOther)
}

func (h *UserProfileHandler) handleNotifyToggle(w http.ResponseWriter, r *http.Request, uid string) {
	enabled := r.Form.Get("enabled") == "on"
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var chatID *int64
	if err := h.DB.QueryRow(ctx, `select telegram_chat_id from users where id = $1::uuid`, uid).Scan(&chatID); err != nil {
		http.Redirect(w, r, "/profile?notify=error", http.StatusSeeOther)
		return
	}
	if chatID == nil || *chatID == 0 {
		http.Redirect(w, r, "/profile?notify=notlinked", http.StatusSeeOther)
		return
	}
	if _, err := h.DB.Exec(ctx, `update users set telegram_notify = $2 where id = $1::uuid`, uid, enabled); err != nil {
		http.Redirect(w, r, "/profile?notify=error", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/profile?notify=updated", http.StatusSeeOther)
}

func (h *UserProfileHandler) handleTransfer(w http.ResponseWriter, r *http.Request, uid string) {
	recipientUsername := strings.TrimSpace(strings.ToLower(r.Form.Get("recipient")))
	if recipientUsername == "" {
		http.Redirect(w, r, "/profile?transfer=missing", http.StatusSeeOther)
		return
	}
	amountStr := strings.TrimSpace(r.Form.Get("amount"))
	amount, err := strconv.ParseInt(amountStr, 10, 64)
	if err != nil || amount <= 0 {
		http.Redirect(w, r, "/profile?transfer=invalid", http.StatusSeeOther)
		return
	}
	note := strings.TrimSpace(r.Form.Get("note"))
	if len([]rune(note)) > 200 {
		note = string([]rune(note)[:200])
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var (
		senderDisplay  string
		recipientID    string
		recipientName  string
		senderAcct     string
		recipientAcct  string
		currentBalance int64
	)

	if err := h.DB.QueryRow(ctx, `select display_name from users where id = $1::uuid`, uid).Scan(&senderDisplay); err != nil {
		http.Redirect(w, r, "/profile?transfer=error", http.StatusSeeOther)
		return
	}
	if err := h.DB.QueryRow(ctx, `
		select id::text, display_name
		from users where lower(username) = $1
	`, recipientUsername).Scan(&recipientID, &recipientName); err != nil {
		http.Redirect(w, r, "/profile?transfer=unknown", http.StatusSeeOther)
		return
	}
	if recipientID == uid {
		http.Redirect(w, r, "/profile?transfer=self", http.StatusSeeOther)
		return
	}
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		http.Redirect(w, r, "/profile?transfer=error", http.StatusSeeOther)
		return
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	if err := tx.QueryRow(ctx, `select id::text from accounts where user_id = $1::uuid and is_default for update`, uid).Scan(&senderAcct); err != nil {
		http.Redirect(w, r, "/profile?transfer=error", http.StatusSeeOther)
		return
	}
	if err := tx.QueryRow(ctx, `select id::text from accounts where user_id = $1::uuid and is_default`, recipientID).Scan(&recipientAcct); err != nil {
		http.Redirect(w, r, "/profile?transfer=error", http.StatusSeeOther)
		return
	}

	err = tx.QueryRow(ctx, `select coalesce(balance,0)::bigint from user_balances where user_id = $1::uuid`, uid).Scan(&currentBalance)
	if err == pgx.ErrNoRows {
		currentBalance = 0
	} else if err != nil {
		http.Redirect(w, r, "/profile?transfer=error", http.StatusSeeOther)
		return
	}
	if amount > currentBalance {
		http.Redirect(w, r, "/profile?transfer=notenough", http.StatusSeeOther)
		return
	}

	var txID string
	if err := tx.QueryRow(ctx, `
		insert into transactions (reason, note)
		values ('TRANSFER', nullif($1,''))
		returning id::text
	`, note).Scan(&txID); err != nil {
		http.Redirect(w, r, "/profile?transfer=error", http.StatusSeeOther)
		return
	}
	if _, err := tx.Exec(ctx, `
		insert into ledger_entries (tx_id, account_id, delta) values
		($1,$2,$4), ($1,$3,$5)
	`, txID, senderAcct, recipientAcct, -amount, amount); err != nil {
		http.Redirect(w, r, "/profile?transfer=error", http.StatusSeeOther)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Redirect(w, r, "/profile?transfer=error", http.StatusSeeOther)
		return
	}
	tx = nil

	summary := fmt.Sprintf("🦶 %d PiedPièces", amount)
	if note != "" {
		summary += "\nNote: " + note
	}
	h.Notifier.NotifyUser(ctx, uid, fmt.Sprintf("You sent %s to %s.", summary, recipientName))
	h.Notifier.NotifyUser(ctx, recipientID, fmt.Sprintf("%s sent you %s.", senderDisplay, summary))

	http.Redirect(w, r, "/profile?transfer=sent", http.StatusSeeOther)
}
func (h *UserProfileHandler) fetchUserOptions(ctx context.Context) ([]profileUserOption, error) {
	rows, err := h.DB.Query(ctx, `
		select username, display_name
		from users
		order by display_name asc
		limit 200
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var opts []profileUserOption
	for rows.Next() {
		var opt profileUserOption
		if err := rows.Scan(&opt.Username, &opt.DisplayName); err != nil {
			return nil, err
		}
		opts = append(opts, opt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return opts, nil
}

func (h *UserProfileHandler) updateUserRole(ctx context.Context, adminID, targetUsername, newRole string) (string, error) {
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var targetID, oldRole, displayName string
	if err := tx.QueryRow(ctx, `
		select id::text, role, display_name
		from users
		where username = $1
		for update
	`, targetUsername).Scan(&targetID, &oldRole, &displayName); err != nil {
		return "", err
	}

	if oldRole != newRole {
		if _, err := tx.Exec(ctx, `
			update users
			set role = $1
			where id = $2::uuid
		`, newRole, targetID); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `
			insert into admin_actions (admin_user_id, target_user_id, action, old_role, new_role)
			values ($1::uuid, $2::uuid, $3, $4, $5)
		`, adminID, targetID, "role_change", oldRole, newRole); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return displayName, nil
}

func isValidRole(role string) bool {
	switch role {
	case middleware.RoleUnverified, middleware.RoleUser, middleware.RoleModerator, middleware.RoleAdmin:
		return true
	default:
		return false
	}
}
