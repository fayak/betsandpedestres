package http

import (
	"time"

	"betsandpedestres/internal/notify"
	"betsandpedestres/internal/web"
	"github.com/jackc/pgx/v5/pgxpool"
)

type BetNewHandler struct {
	DB  *pgxpool.Pool
	TPL *web.Renderer
}

type betNewContent struct {
	Title string
}

type BetWagerCreateHandler struct {
	DB       *pgxpool.Pool
	Notifier notify.Notifier
	BaseURL  string
}

type bettorVM struct {
	Name     string
	Username string
	Amount   int64
}

type betOptionVM struct {
	ID           string
	Label        string
	Stakes       int64
	Bettors      []bettorVM
	Ratio        string
	Percent      int
	SelectedByMe bool
}

type betShowContent struct {
	BetID           string
	Title           string
	Description     *string
	ExternalURL     *string
	Deadline        *time.Time
	DeadlineDefined bool
	Options         []betOptionVM
	TotalStakes     int64
	CreatorName     string
	CreatorUsername string

	CanWager          bool
	MaxStake          int64 // user's current balance (server-enforced too)
	IdempotencyKey    string
	ResolutionAllowed bool

	ResolutionMode      bool
	IsModerator         bool
	AlreadyClosed       bool
	PastDeadline        bool
	WaitingForConsensus bool
	WaitingForAdmin     bool
	StatusLabel         string // "Open" | "Past deadline" | "Resolution in progress" | "Closed"
	VotesTotal          int
	Quorum              int
	MyVoteOptionID      *string
	MyVoteLabel         *string
	WinningOptionID     *string
	WinningLabel        *string

	Payouts []payoutVM
}

type payoutVM struct {
	Name     string
	Username string
	Amount   int64
}

type BetShowHandler struct {
	DB     *pgxpool.Pool
	TPL    *web.Renderer
	Quorum int
}
