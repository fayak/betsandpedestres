package http

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"betsandpedestres/internal/http/middleware"
	"betsandpedestres/internal/web"

	"github.com/jackc/pgx/v5/pgxpool"
)

type TransactionsHandler struct {
	DB  *pgxpool.Pool
	TPL *web.Renderer
}

type TxEntry struct {
	AccountID   string
	UserID      *string
	Delta       int64
	DisplayName *string
	AccountKind string
}

type TxRow struct {
	ID        string
	Reason    string
	BetID     *string
	BetTitle  *string
	Note      *string
	CreatedAt time.Time
	PrevHash  *string
	Hash      string
	Entries   []TxEntry

	// Derived for UI within this page:
	ChainOK bool // does this row's prev_hash == previous row's hash
}

type TxContent struct {
	Rows      []TxRow
	Page      int
	Size      int
	HasPrev   bool
	HasNext   bool
	PrevURL   string
	NextURL   string
	OverallOK bool
	Title     string
}

type userLite struct {
	ID          string
	Username    string
	DisplayName string
}
type accountLite struct {
	ID     string
	UserID *string
	BetID  *string
	Name   string
}

func (h *TransactionsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)
	header, role := loadHeader(r.Context(), h.DB, uid)
	if !header.LoggedIn || role == middleware.RoleUnverified {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// pagination (defaults)
	pagenb := parseIntDefault(r.URL.Query().Get("page"), 1)
	if pagenb < 1 {
		pagenb = 1
	}
	size := parseIntDefault(r.URL.Query().Get("size"), 50)
	if size < 1 {
		size = 50
	}
	if size > 200 {
		size = 200
	}

	limit := size + 1 // fetch one extra to detect "has next"
	offset := (pagenb - 1) * size

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := h.DB.Query(ctx, `
		select id, reason, bet_id::text, note, created_at, prev_hash_hex, hash_hex, entries
		from public_transactions
		order by created_at desc, id desc
		limit $1 offset $2
	`, limit, offset)
	if err != nil {
		slog.Error("transactions.query", "err", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var list []TxRow
	for rows.Next() {
		var t TxRow
		var betID *string
		var note *string
		var entriesJSON []byte
		if err := rows.Scan(&t.ID, &t.Reason, &betID, &note, &t.CreatedAt, &t.PrevHash, &t.Hash, &entriesJSON); err != nil {
			slog.Error("transactions.scan", "err", err)
			http.Error(w, "scan error", http.StatusInternalServerError)
			return
		}
		t.BetID = betID
		t.Note = note

		ents, err := decodeEntries(entriesJSON)
		if err != nil {
			slog.Error("transactions.decode_entries", "err", err)
			http.Error(w, "decode error", http.StatusInternalServerError)
			return
		}
		t.Entries = ents

		list = append(list, t)

		accIDs := make(map[string]struct{})
		userIDs := make(map[string]struct{})
		for i := range list {
			for _, e := range list[i].Entries {
				accIDs[e.AccountID] = struct{}{}
				if e.UserID != nil {
					userIDs[*e.UserID] = struct{}{}
				}
			}
		}

		// Bulk load accounts
		accIDSlice := make([]string, 0, len(accIDs))
		for id := range accIDs {
			accIDSlice = append(accIDSlice, id)
		}

		accMap := map[string]accountLite{}
		if len(accIDSlice) > 0 {
			rows2, err := h.DB.Query(ctx, `
		select id::text, user_id::text, bet_id::text, name
		from accounts
		where id = any($1::uuid[])
	`, accIDSlice)
			if err != nil {
				slog.Error("transactions.accounts.query", "err", err)
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
			for rows2.Next() {
				var a accountLite
				var userID, betID *string
				if err := rows2.Scan(&a.ID, &userID, &betID, &a.Name); err != nil {
					slog.Error("transactions.accounts.scan", "err", err)
					http.Error(w, "db error", http.StatusInternalServerError)
					return
				}
				a.UserID = userID
				a.BetID = betID
				accMap[a.ID] = a
				if userID != nil {
					userIDs[*userID] = struct{}{}
				}
			}
			if err := rows2.Err(); err != nil {
				slog.Error("transactions.accounts.rows_err", "err", err)
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
		}

		// Bulk load users we need (to map id -> username/display_name)
		userIDSlice := make([]string, 0, len(userIDs))
		for id := range userIDs {
			userIDSlice = append(userIDSlice, id)
		}

		userMap := map[string]userLite{}
		var houseUserID *string
		if len(userIDSlice) > 0 {
			rows3, err := h.DB.Query(ctx, `
		select id::text, username, display_name
		from users
		where id = any($1::uuid[])
	`, userIDSlice)
			if err != nil {
				slog.Error("transactions.users.query", "err", err)
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
			for rows3.Next() {
				var u userLite
				if err := rows3.Scan(&u.ID, &u.Username, &u.DisplayName); err != nil {
					slog.Error("transactions.users.scan", "err", err)
					http.Error(w, "db error", http.StatusInternalServerError)
					return
				}
				if u.Username == "house" {
					houseUserID = &u.ID
				}
				userMap[u.ID] = u
			}
			if err := rows3.Err(); err != nil {
				slog.Error("transactions.users.rows_err", "err", err)
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
		}

		// Transform entries per tx: add display names & filter negative house debits
		for i := range list {
			enriched := make([]TxEntry, 0, len(list[i].Entries))
			for _, e := range list[i].Entries {
				acc := accMap[e.AccountID]

				if acc.UserID != nil {
					// user wallet entry
					u := userMap[*acc.UserID]
					// Skip negative house line
					if houseUserID != nil && *acc.UserID == *houseUserID && e.Delta < 0 {
						continue
					}
					name := u.DisplayName
					e.DisplayName = &name
					e.AccountKind = "wallet"
				} else {
					// escrow account (bet)
					e.AccountKind = "escrow"
				}
				enriched = append(enriched, e)
			}
			list[i].Entries = enriched
		}
	}

	if err := rows.Err(); err != nil {
		slog.Error("transactions.rows_err", "err", err)
		http.Error(w, "db rows error", http.StatusInternalServerError)
		return
	}
	betIDs := make(map[string]struct{})
	for i := range list {
		if list[i].BetID != nil {
			betIDs[*list[i].BetID] = struct{}{}
		}
	}
	if len(betIDs) > 0 {
		idSlice := make([]string, 0, len(betIDs))
		for id := range betIDs {
			idSlice = append(idSlice, id)
		}

		bt := map[string]string{}
		rowsB, err := h.DB.Query(ctx, `
		select id::text, title
		from bets
		where id = any($1::uuid[])
	`, idSlice)
		if err != nil {
			slog.Error("transactions.bets.query", "err", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		for rowsB.Next() {
			var id, title string
			if err := rowsB.Scan(&id, &title); err != nil {
				slog.Error("transactions.bets.scan", "err", err)
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
			bt[id] = title
		}
		if err := rowsB.Err(); err != nil {
			slog.Error("transactions.bets.rows_err", "err", err)
			http.Error(w, "db rows error", http.StatusInternalServerError)
			return
		}
		for i := range list {
			if list[i].BetID != nil {
				if t, ok := bt[*list[i].BetID]; ok {
					list[i].BetTitle = &t
				}
			}
		}
	}

	hasNext := false
	if len(list) > size {
		hasNext = true
		list = list[:size]
	}

	overallOK := true
	for i := range list {
		if i+1 >= len(list) {
			list[i].ChainOK = true
			continue
		}
		ok := (list[i].PrevHash != nil && *list[i].PrevHash == list[i+1].Hash)
		list[i].ChainOK = ok
		if !ok {
			overallOK = false
		}
	}

	content := TxContent{
		Rows:      list,
		Page:      pagenb,
		Size:      size,
		HasPrev:   pagenb > 1,
		HasNext:   hasNext,
		PrevURL:   "/transactions?page=" + itoa(pagenb-1) + "&size=" + itoa(size),
		NextURL:   "/transactions?page=" + itoa(pagenb+1) + "&size=" + itoa(size),
		OverallOK: overallOK,
		Title:     "All transactions",
	}

	page := web.Page[TxContent]{Header: header, Content: content}

	var buf bytes.Buffer
	if err := h.TPL.Render(&buf, "transactions", page); err != nil {
		slog.Error("template error", "error", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

type entryJSON struct {
	AccountID string  `json:"account_id"`
	UserID    *string `json:"user_id"`
	Delta     int64   `json:"delta"`
}

func decodeEntries(b []byte) ([]TxEntry, error) {
	var raw []entryJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	out := make([]TxEntry, len(raw))
	for i, e := range raw {
		out[i] = TxEntry{
			AccountID: e.AccountID,
			UserID:    e.UserID,
			Delta:     e.Delta,
		}
	}
	return out, nil
}
