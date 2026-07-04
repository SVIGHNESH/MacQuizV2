package authusers

import (
	"testing"
	"time"
)

func TestAccessTokenRoundTrip(t *testing.T) {
	secret := []byte("test-secret")
	token, err := signAccessToken(secret, "user-123", "teacher", time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	userID, role, err := parseAccessToken(secret, token)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if userID != "user-123" || role != "teacher" {
		t.Fatalf("claims = (%q, %q), want (user-123, teacher)", userID, role)
	}
}

func TestAccessTokenRejectsWrongSecret(t *testing.T) {
	token, _ := signAccessToken([]byte("secret-a"), "u", "admin", time.Now())
	if _, _, err := parseAccessToken([]byte("secret-b"), token); err == nil {
		t.Fatal("token signed with a different secret verified")
	}
}

func TestAccessTokenExpires(t *testing.T) {
	secret := []byte("test-secret")
	// Issued long enough ago that the 15-minute TTL has passed.
	token, _ := signAccessToken(secret, "u", "student", time.Now().Add(-AccessTokenTTL-time.Minute))
	if _, _, err := parseAccessToken(secret, token); err == nil {
		t.Fatal("expired token verified")
	}
}

func TestAccessTokenRejectsGarbage(t *testing.T) {
	if _, _, err := parseAccessToken([]byte("s"), "not.a.jwt"); err == nil {
		t.Fatal("garbage token verified")
	}
}
