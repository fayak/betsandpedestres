package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

var secret []byte

// Call this once at startup with cfg.Security.JWTSecret
func SetSecret(s string) {
	secret = []byte(s)
}

// Hash & check
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}
func CheckPassword(pw, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// JWT
func IssueToken(userID string) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("jwt secret not set")
	}
	claims := jwt.MapClaims{
		"sub": userID,
		"exp": time.Now().Add(72 * time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString(secret)
}

func ParseToken(tok string) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("jwt secret not set")
	}
	parsed, err := jwt.Parse(tok, func(t *jwt.Token) (interface{}, error) { return secret, nil })
	if err != nil || !parsed.Valid {
		return "", errors.New("invalid token")
	}
	if c, ok := parsed.Claims.(jwt.MapClaims); ok {
		if sub, ok := c["sub"].(string); ok {
			return sub, nil
		}
	}
	return "", errors.New("no sub")
}
