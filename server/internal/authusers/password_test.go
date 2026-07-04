package authusers

import (
	"strings"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Fatalf("hash %q is not PHC argon2id format", hash)
	}

	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Fatalf("verify correct password = (%v, %v), want (true, nil)", ok, err)
	}
	ok, err = VerifyPassword("wrong password", hash)
	if err != nil || ok {
		t.Fatalf("verify wrong password = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestHashPasswordUsesUniqueSalts(t *testing.T) {
	a, _ := HashPassword("same input")
	b, _ := HashPassword("same input")
	if a == b {
		t.Fatal("two hashes of the same password are identical; salt is not random")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	for _, bad := range []string{
		"", "plaintext", "$bcrypt$whatever",
		"$argon2id$v=19$m=19456,t=2,p=1$notbase64!!$alsobad!!",
	} {
		if _, err := VerifyPassword("x", bad); err == nil {
			t.Errorf("VerifyPassword accepted malformed hash %q", bad)
		}
	}
}

func TestTimingDecoyHashIsWellFormed(t *testing.T) {
	ok, err := VerifyPassword("any password", timingDecoyHash)
	if err != nil {
		t.Fatalf("decoy hash does not parse: %v", err)
	}
	if ok {
		t.Fatal("decoy hash matched a password; it must match nothing")
	}
}
