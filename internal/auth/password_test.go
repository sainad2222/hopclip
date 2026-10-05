package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected encoding %q", h)
	}
	if err := VerifyPassword(h, "correct horse"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := VerifyPassword(h, "correct horsE"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("wrong password: %v", err)
	}
	h2, _ := HashPassword("correct horse")
	if h == h2 {
		t.Fatal("salt not random")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	for _, h := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$AA$AA", "$argon2id$v=18$m=1,t=1,p=1$AA$AA", "$argon2id$v=19$garbage$AA$AA", "$argon2id$v=19$m=1,t=1,p=1$!!$AA"} {
		if err := VerifyPassword(h, "x"); err == nil {
			t.Errorf("accepted %q", h)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if ValidatePassword("1234567") == nil {
		t.Error("7 chars accepted")
	}
	if ValidatePassword("12345678") != nil {
		t.Error("8 chars rejected")
	}
	if ValidatePassword("ééééééé") == nil {
		t.Error("length must count characters, not bytes")
	}
	if ValidatePassword(strings.Repeat("a", MaxPasswordLen+1)) == nil {
		t.Error("overlong accepted")
	}
}
