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
	"github.com/jackc/pgx/v5/pgxpool"
)

type HomeHandler struct {
	DB  *pgxpool.Pool
	TPL *web.Renderer
}

type betCard struct {
	ID           string
	Title        string
	CreatorName  string
	CreatorUser  string
	CreatedAt    time.Time
	Deadline     *time.Time
	Stakes       int64
	Participants int64
	Options      []string
}

type creatorOpt struct {
	Username    string
	DisplayName string
}

type homeContent struct {
	Title       string
	Rows        []betCard
	Page        int
	Size        int
	HasPrev     bool
	HasNext     bool
	PrevURL     string
	NextURL     string
	Sort        string
	UserFilter  string // creator username ("" = all)
	PartFilter  string // "all"|"me"|"notme"
	SortChoices []struct{ Key, Label string }
	Creators    []creatorOpt
}

func (h *HomeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)

	// Header (unchanged)
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

	// Controls
	q := r.URL.Query()
	page := atoiDefault(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	size := atoiDefault(q.Get("size"), 20)
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	sort := q.Get("sort")
	if sort == "" {
		sort = "created_desc"
	}
	userFilter := strings.TrimSpace(q.Get("user")) // creator username
	partFilter := strings.TrimSpace(q.Get("p"))    // "all","me","notme"
	if partFilter == "" {
		partFilter = "all"
	}

	orderBy := `order by b.created_at desc, b.id desc`
	switch sort {
	case "created_asc":
		orderBy = `order by b.created_at asc, b.id asc`
	case "deadline_asc":
		orderBy = `order by b.deadline asc nulls last, b.id asc`
	case "deadline_desc":
		orderBy = `order by b.deadline desc nulls last, b.id desc`
	case "most_stakes":
		orderBy = `order by coalesce(sum_w,0) desc, b.created_at desc, b.id desc`
	case "least_stakes":
		orderBy = `order by coalesce(sum_w,0) asc, b.created_at desc, b.id desc`
	case "participants_desc":
		orderBy = `order by coalesce(participants,0) desc, b.created_at desc, b.id desc`
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// 1) Creator dropdown options (distinct creators of open bets)
	var creators []creatorOpt
	{
		rows, err := h.DB.Query(ctx, `
			select distinct u.username, u.display_name
			from bets b
			join users u on u.id = b.creator_user_id
			where b.status = 'open'
			order by u.display_name asc
		`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var c creatorOpt
				if err := rows.Scan(&c.Username, &c.DisplayName); err != nil {
					break
				}
				creators = append(creators, c)
			}
		}
	}

	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	// CTE WHERE: only tables present in CTE (bets b, wagers w)
	whereAgg := `where b.status = 'open'`

	// Outer WHERE (can reference users u, and participation EXISTS)
	whereOuter := `where b.status = 'open'`
	if userFilter != "" {
		whereOuter += ` and u.username = ` + arg(userFilter)
	}
	if uid != "" && partFilter != "all" {
		if partFilter == "me" {
			whereOuter += ` and exists (
			select 1 from wagers w where w.bet_id = b.id and w.user_id = ` + arg(uid) + `
		)`
		} else if partFilter == "notme" {
			whereOuter += ` and not exists (
			select 1 from wagers w where w.bet_id = b.id and w.user_id = ` + arg(uid) + `
		)`
		}
	}

	limit := size + 1
	offset := (page - 1) * size
	limitPH := arg(limit)
	offsetPH := arg(offset)

	// Final SQL
	sql := `
with agg as (
  select
    b.id,
    sum(w.amount)::bigint as sum_w,
    count(distinct w.user_id)::bigint as participants
  from bets b
  left join wagers w on w.bet_id = b.id
  ` + whereAgg + `
  group by b.id
)
select
  b.id::text,
  b.title,
  u.display_name as creator_name,
  u.username     as creator_username,
  b.created_at,
  b.deadline,
  coalesce(a.sum_w, 0)        as stakes,
  coalesce(a.participants, 0) as participants,
  (select array_agg(label order by position asc) from bet_options bo where bo.bet_id = b.id) as opts
from bets b
join users u on u.id = b.creator_user_id
left join agg a on a.id = b.id
` + whereOuter + `
` + orderBy + `
limit ` + limitPH + `::int offset ` + offsetPH + `::int
`
	rows, err := h.DB.Query(ctx, sql, args...)

	if err != nil {
		slog.Error("db error", "error", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var list []betCard
	for rows.Next() {
		var bc betCard
		var opts []string
		if err := rows.Scan(&bc.ID, &bc.Title, &bc.CreatorName, &bc.CreatorUser, &bc.CreatedAt, &bc.Deadline, &bc.Stakes, &bc.Participants, &opts); err != nil {
			http.Error(w, "scan error", http.StatusInternalServerError)
			return
		}
		bc.Options = opts
		list = append(list, bc)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "db rows error", http.StatusInternalServerError)
		return
	}

	hasNext := false
	if len(list) > size {
		hasNext = true
		list = list[:size]
	}

	choices := []struct{ Key, Label string }{
		{"created_desc", "Latest created"},
		{"created_asc", "Earliest created"},
		{"deadline_asc", "Earliest deadline"},
		{"deadline_desc", "Latest deadline"},
		{"most_stakes", "Most stakes"},
		{"least_stakes", "Least stakes"},
		{"participants_desc", "Most participants"},
	}

	content := homeContent{
		Title:       "Active bets",
		Rows:        list,
		Page:        page,
		Size:        size,
		HasPrev:     page > 1,
		HasNext:     hasNext,
		PrevURL:     buildURL("/?page="+itoa(page-1)+"&size="+itoa(size)+"&sort="+sort, userFilter, partFilter),
		NextURL:     buildURL("/?page="+itoa(page+1)+"&size="+itoa(size)+"&sort="+sort, userFilter, partFilter),
		Sort:        sort,
		UserFilter:  userFilter,
		PartFilter:  partFilter,
		SortChoices: choices,
		Creators:    creators,
	}

	pageVM := web.Page[homeContent]{Header: header, Content: content}

	var buf bytes.Buffer
	if err := h.TPL.Render(&buf, "home", pageVM); err != nil {
		slog.Error("could not render", "error", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func buildURL(base, user, p string) string {
	var sb strings.Builder
	sb.WriteString(base)
	if strings.Contains(base, "?") {
		sb.WriteString("&")
	} else {
		sb.WriteString("?")
	}
	if user != "" {
		sb.WriteString("user=")
		sb.WriteString(user)
		sb.WriteString("&")
	}
	if p != "" {
		sb.WriteString("p=")
		sb.WriteString(p)
		sb.WriteString("&")
	}
	s := sb.String()
	if s[len(s)-1] == '&' {
		s = s[:len(s)-1]
	}
	return s
}

func itoa(n int) string { return strconv.Itoa(n) }
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
