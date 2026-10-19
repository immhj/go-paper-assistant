package auth

import (
	"strings"
	"testing"
	"time"
)

func TestJWTRoundTripIncludesAuthVersion(t *testing.T) {
	issuer := NewJWT("test-secret", time.Hour)
	token, err := issuer.Issue("u_abc123", 7)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	claims, err := issuer.Verify(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Username != "u_abc123" || claims.AuthVersion != 7 {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	if claims.Exp <= claims.Iat {
		t.Fatalf("expiry must follow issue time: %+v", claims)
	}
}

func TestJWTRejectsExpired(t *testing.T) {
	issuer := NewJWT("test-secret", -time.Minute)
	token, err := issuer.Issue("u_x", 1)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := issuer.Verify(token); err == nil {
		t.Fatal("expired token verified")
	}
}

func TestJWTRejectsWrongSecret(t *testing.T) {
	token, _ := NewJWT("secret-a", time.Hour).Issue("u_x", 1)
	if _, err := NewJWT("secret-b", time.Hour).Verify(token); err == nil {
		t.Fatal("token verified under a different secret")
	}
}

func TestJWTRejectsTamperedPayload(t *testing.T) {
	issuer := NewJWT("test-secret", time.Hour)
	token, _ := issuer.Issue("u_x", 1)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT segments, got %d", len(parts))
	}
	tampered := parts[0] + "." + parts[1] + "x" + "." + parts[2]
	if _, err := issuer.Verify(tampered); err == nil {
		t.Fatal("tampered token verified")
	}
}

func TestJWTRejectsMissingIdentityOrVersion(t *testing.T) {
	issuer := NewJWT("test-secret", time.Hour)
	if _, err := issuer.Issue("", 1); err == nil {
		t.Fatal("empty username accepted")
	}
	if _, err := issuer.Issue("u_x", 0); err == nil {
		t.Fatal("zero auth version accepted")
	}
}

func TestJWTRejectsGarbage(t *testing.T) {
	issuer := NewJWT("test-secret", time.Hour)
	for _, bad := range []string{"", "a.b", "a.b.c.d", "not-a-token"} {
		if _, err := issuer.Verify(bad); err == nil {
			t.Fatalf("garbage token %q verified", bad)
		}
	}
}
