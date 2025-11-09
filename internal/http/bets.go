package http

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"betsandpedestres/internal/http/middleware"
	"betsandpedestres/internal/web"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

type BetShowHandler struct {
	DB  *pgxpool.Pool
	TPL *web.Renderer
}

func (h *BetShowHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)

	header := web.HeaderData{}
	if uid != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		_ = h.DB.QueryRow(ctx, `
			select u.username, u.display_name, coalesce(b.balance,0)
			from users u
			left join user_balances b on b.user_id = u.id
			where u.id = $1
		`, uid).Scan(&header.Username, &header.DisplayName, &header.Balance)
		if header.Username != "" {
			header.LoggedIn = true
		}
	}

	betID := r.PathValue("id")
	if betID == "" {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	modeResolve := r.URL.Query().Get("mode") == "resolve"

	isMod := false
	if header.LoggedIn {
		ok, err := middleware.IsModerator(ctx, h.DB, uid)
		if err == nil {
			isMod = ok
		} else {
			slog.Warn("could not determine if is_mod", "error", err)
		}
	}

	// fetch bet (add these columns)
	var title, creatorName string
	var desc, ext *string
	var deadline *time.Time
	var winning *string
	var status string

	err := h.DB.QueryRow(ctx, `
  select b.title, u.display_name, b.description, b.external_url, b.deadline, b.resolution_option_id::text, b.status
  from bets b
  join users u on u.id = b.creator_user_id
  where b.id = $1::uuid
`, betID).Scan(&title, &creatorName, &desc, &ext, &deadline, &winning, &status)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "db error", http.StatusInternalServerError)
		slog.Error("db error", "error", err)
		return
	}

	rows, err := h.DB.Query(ctx, `
  select
    bo.id::text,
    bo.label,
    coalesce( (select sum(w3.amount)::bigint from wagers w3 where w3.option_id = bo.id), 0 ) as stakes,
    coalesce( array_agg(wl.display_name order by wl.amt desc)
              filter (where wl.display_name is not null), '{}' ) as bettor_names,
    coalesce( array_agg(wl.amt        order by wl.amt desc)
              filter (where wl.display_name is not null), '{}' ) as bettor_amts
  from bet_options bo
  left join lateral (
    select u.display_name, sum(w2.amount)::bigint as amt
    from wagers w2
    join users u on u.id = w2.user_id
    where w2.option_id = bo.id
    group by u.display_name
    order by amt desc
  ) wl on true
  where bo.bet_id = $1::uuid
  group by bo.id, bo.label
  order by bo.position asc
`, betID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var opts []betOptionVM
	for rows.Next() {
		var o betOptionVM
		var names []string
		var amts []int64
		if err := rows.Scan(&o.ID, &o.Label, &o.Stakes, &names, &amts); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		// zip bettors
		n := len(names)
		if len(amts) < n {
			n = len(amts)
		}
		o.Bettors = make([]bettorVM, 0, n)
		for i := 0; i < n; i++ {
			o.Bettors = append(o.Bettors, bettorVM{Name: names[i], Amount: amts[i]})
		}
		opts = append(opts, o)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "db rows error", http.StatusInternalServerError)
		return
	}

	alreadyClosed := (status != "open") || (winning != nil)

	// compute user's max stake
	var maxStake int64
	canWager := header.LoggedIn && !modeResolve && !alreadyClosed
	if canWager {
		_ = h.DB.QueryRow(ctx, `select coalesce(balance,0) from user_balances where user_id = $1`, uid).Scan(&maxStake)
	}
	if header.LoggedIn {
		_ = h.DB.QueryRow(ctx, `select coalesce(balance,0) from user_balances where user_id = $1`, header.Username /* wrong: needs user_id */).Scan(new(any))
		// Use uid (user id), not username:
		_ = h.DB.QueryRow(ctx, `select coalesce(balance,0) from user_balances where user_id = $1`, uid).Scan(&maxStake)
	}

	// idempotency token for the form
	idk := randomHex(16)

	// build options + bettors (same query you have), then:
	var total int64
	for _, o := range opts {
		total += o.Stakes
	}
	for i := range opts {
		opts[i].Ratio = computeRatio(opts[i].Stakes, total-opts[i].Stakes)
	}

	content := betShowContent{
		BetID:           betID,
		Title:           title,
		Description:     desc,
		ExternalURL:     ext,
		Deadline:        deadline,
		Options:         opts,
		TotalStakes:     total,
		CreatorName:     creatorName,
		CanWager:        canWager,
		MaxStake:        maxStake,
		IdempotencyKey:  idk,
		ResolutionMode:  modeResolve && isMod && !alreadyClosed,
		IsModerator:     isMod,
		AlreadyClosed:   alreadyClosed,
		WinningOptionID: winning,
	}

	page := web.Page[betShowContent]{Header: header, Content: content}

	var buf bytes.Buffer
	if err := h.TPL.Render(&buf, "bet_show", page); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func gcd64(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}
func computeRatio(a, b int64) string {
	// edge cases
	if a == 0 && b == 0 {
		return "—"
	}
	if a == 0 {
		return "0:1"
	}
	if b == 0 {
		return "1:0"
	}
	g := gcd64(a, b)
	return strconv.FormatInt(a/g, 10) + ":" + strconv.FormatInt(b/g, 10)
}
