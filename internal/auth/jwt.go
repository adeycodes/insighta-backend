package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is the JWT payload stored in every access token.
type Claims struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

// AccessTokenTTL is kept intentionally short. Clients must refresh proactively.
const AccessTokenTTL = 3 * time.Minute
const RefreshTokenTTL = 5 * time.Minute

func jwtSecret() []byte {
	s := os.Getenv("JWT_SECRET")
	if s == "" {
		panic("JWT_SECRET is not set — refusing to start")
	}
	return []byte(s)
}

// IssueAccessToken signs a new JWT for the given user.
func IssueAccessToken(userID, username, role string) (string, error) {
	claims := Claims{
		UserID:   userID,
		Username: username,
		Role:     role,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(AccessTokenTTL)),
			Issuer:    "insighta",
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret())
}

// ParseAccessToken validates a signed JWT and returns its claims.
func ParseAccessToken(raw string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(raw, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing algorithm: %v", t.Header["alg"])
		}
		return jwtSecret(), nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("malformed token claims")
	}
	return claims, nil
}

// NewRefreshToken generates a cryptographically random opaque token.
func NewRefreshToken() (string, error) {
	return randomHex(32)
}

// NewState generates a random OAuth state parameter.
func NewState() (string, error) {
	return randomHex(16)
}

// NewAuthCode generates a short-lived one-time code for CLI token exchange.
func NewAuthCode() (string, error) {
	return randomHex(24)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand.Read: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// IssueLongLivedToken issues a JWT that expires in 1 year.
// Used only for seeding test tokens for submission/grading.
func IssueLongLivedToken(userID, username, role string) (string, error) {
	claims := Claims{
		UserID:   userID,
		Username: username,
		Role:     role,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(365 * 24 * time.Hour)),
			Issuer:    "insighta",
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret())
}
