package http

import (
	"context"
	"net/http"
	"strings"
	"time"

	"betsandpedestres/internal/http/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type BetResolveHandler struct{ DB *pgxpool.Pool }

func (h *BetResolveHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)
	if uid == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Must be moderator
	isMod, err := middleware.IsModerator(ctx, h.DB, uid)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if !isMod {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	betID := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	optionID := strings.TrimSpace(r.Form.Get("option_id"))
	if betID == "" || optionID == "" {
		http.Error(w, "missing fields", http.StatusBadRequest)
		return
	}

	tx, err := h.DB.Begin(ctx)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	// Validate: bet open and option belongs to bet
	var open bool
	err = tx.QueryRow(ctx, `
	  select (b.status = 'open') and (b.deadline is null or b.deadline > now() at time zone 'utc')
	  from bets b
	  join bet_options o on o.bet_id = b.id
	  where b.id = $1::uuid and o.id = $2::uuid
	`, betID, optionID).Scan(&open)
	if err != nil {
		http.Error(w, "invalid bet/option", http.StatusBadRequest)
		return
	}
	if !open {
		http.Error(w, "bet not open", http.StatusConflict)
		return
	}

	// Upsert vote (so a moderator can change mind before consensus)
	_, err = tx.Exec(ctx, `
	  insert into bet_resolution_votes (bet_id, user_id, option_id)
	  values ($1::uuid, $2::uuid, $3::uuid)
	  on conflict (bet_id, user_id) do update set option_id = excluded.option_id, created_at = now()
	`, betID, uid, optionID)
	if err != nil {
		http.Error(w, "vote error", http.StatusInternalServerError)
		return
	}

	// Check for consensus:
	// Rule: at least 2 moderator votes, and all votes cast for this bet agree on the same option.
	// (Adjustable later to stricter policies.)
	var votes int
	var agreed bool
	err = tx.QueryRow(ctx, `
	  with v as (
	    select option_id, count(*) as c
	    from bet_resolution_votes
	    where bet_id = $1::uuid
	    group by option_id
	  )
	  select coalesce(sum(c),0) as total_votes,
	         case when count(*) = 1 then true else false end as all_agree
	  from v
	`, betID).Scan(&votes, &agreed)
	if err != nil {
		http.Error(w, "consensus error", http.StatusInternalServerError)
		return
	}

	if votes >= 2 && agreed {
		// find the agreed option
		var winOpt string
		if err := tx.QueryRow(ctx, `
		  select option_id from bet_resolution_votes
		  where bet_id = $1::uuid
		  limit 1
		`, betID).Scan(&winOpt); err != nil {
			http.Error(w, "consensus error", http.StatusInternalServerError)
			return
		}

		// finalize: set bet closed + resolution, and payout escrow proportionally
		if err := finalizeBetPayout(ctx, tx, betID, winOpt); err != nil {
			http.Error(w, "finalization error", http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "commit error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/bets/"+betID, http.StatusSeeOther)
}

func finalizeBetPayout(ctx context.Context, tx pgx.Tx, betID, winningOptionID string) error {
	// Mark bet as closed with resolution
	if _, err := tx.Exec(ctx, `
	  update bets
	  set status = 'closed', resolution_option_id = $2::uuid, resolved_at = now() at time zone 'utc'
	  where id = $1::uuid
	`, betID, winningOptionID); err != nil {
		return err
	}

	// Get escrow account
	var escrowAcctID string
	if err := tx.QueryRow(ctx, `select id::text from accounts where bet_id = $1::uuid`, betID).Scan(&escrowAcctID); err != nil {
		return err
	}

	// Sum escrow balance (from ledger snapshot via user_balances equivalent for account)
	// Simpler: sum of wagers on the bet == escrow total (we can recompute from wagers)
	var escrowTotal int64
	if err := tx.QueryRow(ctx, `
	  select coalesce(sum(amount),0)::bigint
	  from wagers
	  where bet_id = $1::uuid
	`, betID).Scan(&escrowTotal); err != nil {
		return err
	}

	// Winning pot = sum of wagers on winning option
	var winTotal int64
	if err := tx.QueryRow(ctx, `
	  select coalesce(sum(amount),0)::bigint
	  from wagers
	  where bet_id = $1::uuid and option_id = $2::uuid
	`, betID, winningOptionID).Scan(&winTotal); err != nil {
		return err
	}

	// If no winners (winTotal == 0): define policy. We'll transfer back to house.
	if winTotal == 0 {
		// send entire escrow to house
		var houseAcct string
		if err := tx.QueryRow(ctx, `
		  select a.id::text
		  from accounts a
		  join users u on u.id = a.user_id
		  where u.username = 'house' and a.is_default
		  limit 1
		`).Scan(&houseAcct); err != nil {
			return err
		}
		var txID string
		if err := tx.QueryRow(ctx, `insert into transactions (reason, bet_id, note) values ('BET', $1::uuid, 'no winners – to house') returning id::text`, betID).Scan(&txID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
		  insert into ledger_entries (tx_id, account_id, delta)
		  values ($1, $2, -$4), ($1, $3, $4)
		`, txID, escrowAcctID, houseAcct, escrowTotal); err != nil {
			return err
		}
		return nil
	}

	// Compute per-user winning sums
	type win struct {
		UserID string
		Amount int64
	}
	rows, err := tx.Query(ctx, `
	  select user_id::text, sum(amount)::bigint
	  from wagers
	  where bet_id = $1::uuid and option_id = $2::uuid
	  group by user_id
	`, betID, winningOptionID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var winners []win
	for rows.Next() {
		var w win
		if err := rows.Scan(&w.UserID, &w.Amount); err != nil {
			return err
		}
		winners = append(winners, w)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Prepare payouts: proportional, with integer rounding; last payout adjusts remainder
	var txID string
	if err := tx.QueryRow(ctx, `insert into transactions (reason, bet_id, note) values ('BET', $1::uuid, 'payout') returning id::text`, betID).Scan(&txID); err != nil {
		return err
	}

	var distributed int64
	for i, w := range winners {
		share := (escrowTotal * w.Amount) / winTotal
		if i == len(winners)-1 { // last gets remainder adjustment
			share = escrowTotal - distributed
		} else {
			distributed += share
		}

		// user default wallet
		var wallet string
		if err := tx.QueryRow(ctx, `select id::text from accounts where user_id = $1::uuid and is_default`, w.UserID).Scan(&wallet); err != nil {
			return err
		}
		// ledger: escrow -> winner
		if share > 0 {
			if _, err := tx.Exec(ctx, `
			  insert into ledger_entries (tx_id, account_id, delta)
			  values ($1, $2, -$4), ($1, $3, $4)
			`, txID, escrowAcctID, wallet, share); err != nil {
				return err
			}
		}
	}
	return nil
}
