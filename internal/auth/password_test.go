package auth

import "testing"

func TestHashPasswordRoundTrip(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if encoded == "" {
		t.Fatal("encoded hash is empty")
	}
	if !VerifyPassword(encoded, "correct horse battery staple") {
		t.Fatal("correct password did not verify")
	}
}

func TestVerifyPasswordRejectsWrongPassword(t *testing.T) {
	encoded, err := HashPassword("s3cret")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if VerifyPassword(encoded, "wrong") {
		t.Fatal("wrong password verified")
	}
}

func TestHashPasswordSaltsEachCall(t *testing.T) {
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	if a == b {
		t.Fatal("two hashes of the same password are identical; salt is not random")
	}
}

func TestVerifyPasswordRejectsGarbageEncoding(t *testing.T) {
	if VerifyPassword("not-a-valid-phc-string", "whatever") {
		t.Fatal("garbage encoding verified")
	}
	if VerifyPassword("", "whatever") {
		t.Fatal("empty encoding verified")
	}
}
