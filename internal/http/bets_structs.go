package http

import (
	"time"

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
	DB *pgxpool.Pool
}

type bettorVM struct {
	Name   string
	Amount int64
}

type betOptionVM struct {
	ID      string
	Label   string
	Stakes  int64
	Bettors []bettorVM
	Ratio   string
}

type betShowContent struct {
	BetID       string
	Title       string
	Description *string
	ExternalURL *string
	Deadline    *time.Time
	Options     []betOptionVM
	TotalStakes int64
	CreatorName string

	CanWager       bool
	MaxStake       int64 // user's current balance (server-enforced too)
	IdempotencyKey string

	ResolutionMode  bool
	IsModerator     bool
	AlreadyClosed   bool
	WinningOptionID *string
}
