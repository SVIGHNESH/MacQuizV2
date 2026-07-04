package authusers

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AccessTokenTTL is the JWT lifetime (docs/08-security.md section 1: 15 min).
const AccessTokenTTL = 15 * time.Minute

// RefreshTokenTTL bounds how long a session survives without activity.
const RefreshTokenTTL = 14 * 24 * time.Hour

// accessClaims is the JWT payload: just identity and role. Everything else
// (status, must_change_password) is read from the database per request, so a
// deactivated account is locked out within one access-token lifetime at most
// for stale tokens and immediately for fresh requests.
type accessClaims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// signAccessToken issues a 15-minute HS256 JWT for the user.
func signAccessToken(secret []byte, userID, role string, now time.Time) (string, error) {
	claims := accessClaims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(AccessTokenTTL)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

// parseAccessToken validates signature and expiry and returns (userID, role).
func parseAccessToken(secret []byte, token string) (string, string, error) {
	var claims accessClaims
	parsed, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil {
		return "", "", err
	}
	if !parsed.Valid || claims.Subject == "" {
		return "", "", errors.New("invalid token claims")
	}
	return claims.Subject, claims.Role, nil
}
