package http

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
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

type betOptionSummary struct {
	Label   string
	Percent int
}

type betCard struct {
	ID            string
	Title         string
	CreatorName   string
	CreatorUser   string
	CreatedAt     time.Time
	Deadline      *time.Time
	Stakes        int64
	Participants  int64
	Options       []betOptionSummary
	Status        string
	StatusLabel   string
	StatusColor   string
	ExpiresIn     string
	WinningOption *string
	VoteCount     int
	VotesAgree    bool
}

type creatorOpt struct {
	Username    string
	DisplayName string
}

type homeContent struct {
	Title        string
	Rows         []betCard
	Page         int
	Size         int
	HasPrev      bool
	HasNext      bool
	PrevURL      string
	NextURL      string
	Sort         string
	UserFilter   string // creator username ("" = all)
	PartFilter   string // "all"|"me"|"notme"
	ExpiryFilter string
	SortChoices  []struct{ Key, Label string }
	Creators     []creatorOpt

	ShowSignup   bool
	ShowPending  bool
	SignupStatus string
	Role         string
	Description  string
}

func (h *HomeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)
	header, role := loadHeader(r.Context(), h.DB, uid)

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
	expiryFilter := strings.TrimSpace(q.Get("exp"))
	switch expiryFilter {
	case "", "unresolved":
		expiryFilter = "unresolved"
	case "all", "expired", "open", "waiting", "closed":
	default:
		expiryFilter = "unresolved"
	}

	if !header.LoggedIn {
		content := homeContent{
			Title:        "Welcome to Bets & Pedestres",
			ShowSignup:   true,
			SignupStatus: q.Get("signup"),
			Description:  "Bets & Pedestres lets you create friendly prediction markets with transparent escrows and community-driven resolutions.",
		}
		page := web.Page[homeContent]{Header: header, Content: content}
		var buf bytes.Buffer
		if err := h.TPL.Render(&buf, "home", page); err != nil {
			slog.Error("could not render", "error", err)
			http.Error(w, "template error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(buf.Bytes())
		return
	}

	if role == middleware.RoleUnverified {
		content := homeContent{
			Title:       "Account pending approval",
			ShowPending: true,
			Role:        role,
		}
		page := web.Page[homeContent]{Header: header, Content: content}
		var buf bytes.Buffer
		if err := h.TPL.Render(&buf, "home", page); err != nil {
			slog.Error("could not render", "error", err)
			http.Error(w, "template error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(buf.Bytes())
		return
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

	baseFilters := []string{}
	nowExpr := "now() at time zone 'utc'"
	switch expiryFilter {
	case "unresolved":
		baseFilters = append(baseFilters, `(b.status = 'open')`)
	case "open":
		baseFilters = append(baseFilters, `(b.status = 'open' and (b.deadline is null or b.deadline > `+nowExpr+`))`)
	case "expired":
		baseFilters = append(baseFilters, `(b.status = 'open' and b.deadline is not null and b.deadline <= `+nowExpr+`)`)
	case "waiting":
		baseFilters = append(baseFilters, `(b.status = 'open' and exists (select 1 from bet_resolution_votes v where v.bet_id = b.id))`)
	case "closed":
		baseFilters = append(baseFilters, `(b.status <> 'open')`)
	case "all":
		// no filter
	default:
		baseFilters = append(baseFilters, `(b.status = 'open')`)
	}

	whereAgg := "where true"
	if len(baseFilters) > 0 {
		whereAgg = `where ` + strings.Join(baseFilters, " and ")
	}

	whereOuterParts := append([]string{}, baseFilters...)
	if userFilter != "" {
		whereOuterParts = append(whereOuterParts, `u.username = `+arg(userFilter))
	}
	if uid != "" && partFilter != "all" {
		if partFilter == "me" {
			whereOuterParts = append(whereOuterParts, `exists (
			select 1 from wagers w where w.bet_id = b.id and w.user_id = `+arg(uid)+`
		)`)
		} else if partFilter == "notme" {
			whereOuterParts = append(whereOuterParts, `not exists (
			select 1 from wagers w where w.bet_id = b.id and w.user_id = `+arg(uid)+`
		)`)
		}
	}
	whereOuter := "where true"
	if len(whereOuterParts) > 0 {
		whereOuter = `where ` + strings.Join(whereOuterParts, " and ")
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
  (select array_agg(bo.label order by bo.position asc) from bet_options bo where bo.bet_id = b.id) as opt_labels,
  (select array_agg(coalesce(ws.sum_amount,0)::bigint order by bo.position asc)
     from bet_options bo
     left join lateral (
        select coalesce(sum(w.amount),0)::bigint as sum_amount
        from wagers w
        where w.option_id = bo.id
     ) ws on true
     where bo.bet_id = b.id
  ) as opt_stakes,
  b.status,
  (select count(*)::int from bet_resolution_votes v where v.bet_id = b.id) as vote_count,
  (select case when count(distinct option_id) <= 1 then true else false end
     from bet_resolution_votes v where v.bet_id = b.id) as votes_agree,
  b.resolution_option_id::text as winning_option
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
		var optLabels []string
		var optStakes []int64
		if err := rows.Scan(&bc.ID, &bc.Title, &bc.CreatorName, &bc.CreatorUser, &bc.CreatedAt, &bc.Deadline, &bc.Stakes, &bc.Participants, &optLabels, &optStakes, &bc.Status, &bc.VoteCount, &bc.VotesAgree, &bc.WinningOption); err != nil {
			http.Error(w, "scan error", http.StatusInternalServerError)
			return
		}
		bc.Options = buildOptionSummaries(optLabels, optStakes, bc.Stakes)
		decorateBetCard(&bc)
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
		Title:        "Active bets",
		Rows:         list,
		Page:         page,
		Size:         size,
		HasPrev:      page > 1,
		HasNext:      hasNext,
		PrevURL:      buildURL("/?page="+itoa(page-1)+"&size="+itoa(size)+"&sort="+sort, userFilter, partFilter, expiryFilter),
		NextURL:      buildURL("/?page="+itoa(page+1)+"&size="+itoa(size)+"&sort="+sort, userFilter, partFilter, expiryFilter),
		Sort:         sort,
		UserFilter:   userFilter,
		PartFilter:   partFilter,
		ExpiryFilter: expiryFilter,
		SortChoices:  choices,
		Creators:     creators,
		Role:         role,
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

func buildURL(base, user, p, exp string) string {
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
	if exp != "" && exp != "unresolved" {
		sb.WriteString("exp=")
		sb.WriteString(exp)
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

func buildOptionSummaries(labels []string, stakes []int64, total int64) []betOptionSummary {
	n := len(labels)
	if len(stakes) < n {
		n = len(stakes)
	}
	if n == 0 {
		return nil
	}

	var base int64
	if total > 0 {
		base = total
	} else {
		for i := 0; i < n; i++ {
			base += stakes[i]
		}
	}
	opts := make([]betOptionSummary, 0, n)
	for i := 0; i < n; i++ {
		percent := 0
		if base > 0 {
			percent = int(math.Round(float64(stakes[i]) * 100 / float64(base)))
			if percent > 100 {
				percent = 100
			}
		}
		opts = append(opts, betOptionSummary{Label: labels[i], Percent: percent})
	}
	return opts
}

func decorateBetCard(bc *betCard) {
	bc.StatusLabel, bc.StatusColor = statusBadge(bc.Deadline, bc.WinningOption, bc.Status, bc.VoteCount, bc.VotesAgree)
	bc.ExpiresIn = formatExpiresIn(bc.Deadline)
}

func statusBadge(deadline *time.Time, winning *string, status string, votes int, votesAgree bool) (string, string) {
	now := time.Now().UTC()
	pastDeadline := (deadline != nil && deadline.Before(now) && winning == nil && status == "open" && votes == 0)
	waitingConsensus := (votes > 0 && votesAgree && winning == nil && status == "open")
	waitingAdmin := (votes > 0 && !votesAgree && winning == nil && status == "open")
	alreadyClosed := (status != "open") || (winning != nil)

	switch {
	case alreadyClosed:
		return "Closed", "#5c1c1c"
	case waitingAdmin:
		return "Waiting for admin decision", "#7c2d12"
	case waitingConsensus:
		return "Waiting for consensus", "#f97316"
	case pastDeadline:
		return "Past the deadline", "#facc15"
	default:
		return "Open", "#1f6f43"
	}
}

func formatExpiresIn(deadline *time.Time) string {
	if deadline == nil {
		return ""
	}
	now := time.Now().UTC()
	diff := deadline.Sub(now)
	if diff <= 0 {
		return "expired"
	}
	minutes := int(diff.Minutes())
	hours := int(diff.Hours())
	days := hours / 24
	switch {
	case days > 2:
		return fmt.Sprintf("%dd", days)
	case days >= 1:
		hoursRem := hours % 24
		if hoursRem == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd %dh", days, hoursRem)
	case hours >= 1:
		return fmt.Sprintf("%dh", hours)
	default:
		if minutes == 0 {
			minutes = 1
		}
		return fmt.Sprintf("%dm", minutes)
	}
}
