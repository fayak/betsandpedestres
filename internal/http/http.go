package http

import (
	"log/slog"
	"net/http"
	"time"

	"betsandpedestres/internal/config"
	"betsandpedestres/internal/http/middleware"
	"betsandpedestres/internal/notify"
	"betsandpedestres/internal/telegram"
	"betsandpedestres/internal/web"
	"github.com/jackc/pgx/v5/pgxpool"
)

func NewMux(db *pgxpool.Pool, cfg *config.Config) (*http.ServeMux, error) {
	mux := http.NewServeMux()

	rend, err := web.NewRenderer()
	if err != nil {
		return nil, err
	}

	var notifier notify.Notifier = notify.Noop{}
	if cfg.Telegram.BotToken != "" {
		notifier = telegram.New(db, cfg.Telegram.BotToken, cfg.Telegram.GroupChatID)
	}

	mux.Handle("GET /", &HomeHandler{DB: db, TPL: rend})
	mux.Handle("GET /transactions", &TransactionsHandler{DB: db, TPL: rend})
	mux.Handle("GET /bets/new", &BetNewHandler{DB: db, TPL: rend})
	mux.Handle("POST /bets", &BetCreateHandler{DB: db, Notifier: notifier, BaseURL: cfg.BaseURL})
	mux.Handle("GET /bets/{id}", &BetShowHandler{DB: db, TPL: rend, Quorum: cfg.Moderation.Quorum})
	mux.Handle("POST /bets/{id}/wagers", &BetWagerCreateHandler{DB: db})
	mux.Handle("POST /bets/{id}/resolve", &BetResolveHandler{DB: db, Quorum: cfg.Moderation.Quorum, Notifier: notifier, BaseURL: cfg.BaseURL})
	mux.Handle("POST /register", &AccountRegisterHandler{DB: db, Notifier: notifier})
	profileHandler := &UserProfileHandler{DB: db, TPL: rend, Notifier: notifier}
	mux.Handle("GET /profile", profileHandler)
	mux.Handle("GET /profile/{username}", profileHandler)
	mux.Handle("POST /profile/{username}", profileHandler)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	ah := &AuthHandler{DB: db}
	ah.Routes(mux)

	return mux, nil
}

func WithStandardMiddleware(next http.Handler) http.Handler {
	return requestLogger(securityHeaders(middleware.WithAuth(next)))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &wrapWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
		slog.Info("http.request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type wrapWriter struct {
	http.ResponseWriter
	status int
}

func (w *wrapWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
