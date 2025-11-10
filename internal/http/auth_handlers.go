package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"betsandpedestres/internal/auth"
	"betsandpedestres/internal/http/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
)

type AuthHandler struct {
	DB           *pgxpool.Pool
	LoginLimiter *middleware.RateLimiter
}

func (h *AuthHandler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/auth/login", h.Login)
	mux.HandleFunc("POST /api/v1/auth/logout", h.Logout)
	mux.Handle("GET /api/v1/auth/me", middleware.RequireAuth(http.HandlerFunc(h.Me)))
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type meResp struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	if h.LoginLimiter != nil {
		if !h.LoginLimiter.Allow(middleware.ClientIP(r)) {
			http.Error(w, "too many attempts", http.StatusTooManyRequests)
			return
		}
	}
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		http.Error(w, "missing credentials", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	var (
		id, username, displayName, role, passHash string
	)
	err := h.DB.QueryRow(ctx,
		`select id, username, display_name, role, password_hash
		 from users where username = $1`, req.Username).
		Scan(&id, &username, &displayName, &role, &passHash)
	if err != nil || !auth.CheckPassword(req.Password, passHash) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	token, err := auth.IssueToken(id)
	if err != nil {
		http.Error(w, "token error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   false,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(72 * time.Hour),
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   false,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	var resp meResp
	err := h.DB.QueryRow(ctx,
		`select id, username, display_name, role from users where id = $1`, uid).
		Scan(&resp.ID, &resp.Username, &resp.DisplayName, &resp.Role)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
