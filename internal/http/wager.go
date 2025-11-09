package http

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"betsandpedestres/internal/http/middleware"
	"github.com/jackc/pgconn"
	"github.com/jackc/pgx/v5"
)

func (h *BetWagerCreateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)
	if uid == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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

	optionID := strings.TrimSpace(r.Form.Get("option_id"))
	idempKey := strings.TrimSpace(r.Form.Get("idempotency_key"))
	amtStr := strings.TrimSpace(r.Form.Get("amount"))

	amount, err := strconv.ParseInt(amtStr, 10, 64)
	if err != nil || amount <= 0 {
		http.Error(w, "invalid amount", http.StatusBadRequest)
		return
	}
	if optionID == "" || idempKey == "" {
		http.Error(w, "missing fields", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	tx, err := h.DB.Begin(ctx)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	// 1) Validate bet + option belong together and bet open & not past deadline & no votes yet
	var (
		ok          bool
		creatorID   string
		betTitle    string
		optionLabel string
		bettorName  string
	)
	err = tx.QueryRow(ctx, `
		select (b.status = 'open')
		       and (b.deadline is null or b.deadline > now() at time zone 'utc')
		       and not exists (select 1 from bet_resolution_votes v where v.bet_id = b.id) as can_wager,
		       b.creator_user_id::text,
		       b.title,
		       o.label,
		       u.display_name
		from bet_options o
		join bets b on b.id = o.bet_id
		join users u on u.id = $3::uuid
		where o.id = $1 and b.id = $2
	`, optionID, betID, uid).Scan(&ok, &creatorID, &betTitle, &optionLabel, &bettorName)
	if err != nil {
		http.Error(w, "invalid bet or option", http.StatusBadRequest)
		return
	}
	if !ok {
		http.Error(w, "bet is closed, past deadline, or awaiting resolution", http.StatusConflict)
		return
	}

	// 2) Check available balance (nice UX + faster fail); constraint trigger will also protect
	var avail int64
	err = tx.QueryRow(ctx, `select coalesce(balance,0) from user_balances where user_id = $1`, uid).Scan(&avail)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if amount > avail {
		http.Error(w, "insufficient balance", http.StatusForbidden)
		return
	}

	// 3) Ensure bet escrow account exists
	escrowAcctID, err := ensureBetEscrowAccount(ctx, tx, betID)
	if err != nil {
		slog.Error("escrow error", "error", err)
		http.Error(w, "escrow error", http.StatusInternalServerError)
		return
	}

	// 4) Get user's default wallet account id
	var userAcctID string
	if err := tx.QueryRow(ctx, `
		select id::text from accounts where user_id = $1 and is_default
	`, uid).Scan(&userAcctID); err != nil {
		http.Error(w, "account error", http.StatusInternalServerError)
		return
	}

	// 5) Create transaction header
	var txID string
	if err := tx.QueryRow(ctx, `
		insert into transactions (reason, bet_id, note) values ('BET', $1, null) returning id::text
	`, betID).Scan(&txID); err != nil {
		http.Error(w, "tx error", http.StatusInternalServerError)
		return
	}

	// 6) Ledger entries: user -> escrow
	if _, err := tx.Exec(ctx, `
		insert into ledger_entries (tx_id, account_id, delta) values ($1,$2,$3), ($1,$4,$5)
	`, txID, userAcctID, -amount, escrowAcctID, amount); err != nil {
		http.Error(w, "ledger error", http.StatusInternalServerError)
		return
	}

	// 7) Insert the wager with idempotency
	_, err = tx.Exec(ctx, `
		insert into wagers (bet_id, user_id, option_id, amount, created_at, idempotency_key)
		values ($1, $2, $3, $4, now() at time zone 'utc', $5)
	`, betID, uid, optionID, amount, idempKey)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique violation (idempotency)
			// Treat as already successfully processed
			http.Redirect(w, r, "/bets/"+betID+"?note=already_submitted", http.StatusSeeOther)
			return
		}
		http.Error(w, "wager error", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "commit error", http.StatusInternalServerError)
		return
	}

	if h.Notifier != nil {
		link := betLink(h.BaseURL, betID)
		groupMsg := fmt.Sprintf("%s wagered 🦶 %d PiedPièces on \"%s\" (option: %s)\n%s", bettorName, amount, betTitle, optionLabel, link)
		h.Notifier.NotifyGroup(r.Context(), groupMsg)
		if creatorID != "" && creatorID != uid {
			userMsg := fmt.Sprintf("Your bet \"%s\" received a new wager from %s: 🦶 %d PiedPièces on %s.\n%s", betTitle, bettorName, amount, optionLabel, link)
			h.Notifier.NotifyUser(r.Context(), creatorID, userMsg)
		}
	}

	http.Redirect(w, r, "/bets/"+betID+"?note=placed", http.StatusSeeOther)
}

func ensureBetEscrowAccount(ctx context.Context, tx pgx.Tx, betID string) (string, error) {
	var acctID string
	err := tx.QueryRow(ctx,
		`select id::text from accounts where bet_id = $1::uuid limit 1`,
		betID,
	).Scan(&acctID)
	if err == nil {
		return acctID, nil
	}
	if err != nil && err != pgx.ErrNoRows {
		return "", err
	}

	name := "escrow:" + betID
	err = tx.QueryRow(ctx, `
		insert into accounts (user_id, bet_id, name, is_default)
		values (null, $1::uuid, $2, true)
		returning id::text
	`, betID, name).Scan(&acctID)
	return acctID, err
}

func randomHex(n int) string {
	if n <= 0 {
		n = 16
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		ts := time.Now().UnixNano()
		return fmt.Sprintf("%x", ts)
	}
	return fmt.Sprintf("%x", b)
}
