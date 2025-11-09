package http

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"betsandpedestres/internal/http/middleware"
	"betsandpedestres/internal/web"
	"github.com/jackc/pgx/v5/pgxpool"
)

type HallOfFameHandler struct {
	DB  *pgxpool.Pool
	TPL *web.Renderer
}

type hallOfFameRow struct {
	DisplayName string
	Username    string
	Balance     int64
	Escrow      int64
	Total       int64
	Rank        int
}

type hallOfFameContent struct {
	Title string
	Rows  []hallOfFameRow
}

func (h *HallOfFameHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)
	header, _ := loadHeader(r.Context(), h.DB, uid)
	if !header.LoggedIn {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := h.DB.Query(ctx, `
		with escrow as (
			select w.user_id, sum(w.amount)::bigint as escrow_total
			from wagers w
			join bets b on b.id = w.bet_id
			where b.status = 'open'
			group by w.user_id
		)
		select u.display_name,
		       u.username,
		       coalesce(ub.balance,0)::bigint as wallet,
		       coalesce(e.escrow_total,0)::bigint as escrow,
		       coalesce(ub.balance,0)::bigint + coalesce(e.escrow_total,0)::bigint as total
		from users u
		left join user_balances ub on ub.user_id = u.id
		left join escrow e on e.user_id = u.id
		order by total desc, u.display_name asc
		limit 50
	`)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var list []hallOfFameRow
	for rows.Next() {
		var row hallOfFameRow
		if err := rows.Scan(&row.DisplayName, &row.Username, &row.Balance, &row.Escrow, &row.Total); err != nil {
			http.Error(w, "db scan error", http.StatusInternalServerError)
			return
		}
		row.Rank = len(list) + 1
		list = append(list, row)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "db rows error", http.StatusInternalServerError)
		return
	}

	page := web.Page[hallOfFameContent]{
		Header: header,
		Content: hallOfFameContent{
			Title: "PiedPièces Hall of Fame",
			Rows:  list,
		},
	}

	var buf bytes.Buffer
	if err := h.TPL.Render(&buf, "hof", page); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}
