package auth

import (
	"sync"
	"testing"
	"time"
)

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

func TestVerifyDummyPasswordAlwaysFails(t *testing.T) {
	for _, password := range []string{"", "hunter2hunter", "correct horse battery staple"} {
		if VerifyDummyPassword(password) {
			t.Fatalf("dummy verification succeeded for %q", password)
		}
	}
}

func TestVerifyPasswordWaitsForAFreeArgon2Slot(t *testing.T) {
	encoded, err := HashPassword("bounded")
	if err != nil {
		t.Fatal(err)
	}
	// Occupy every slot, as maxConcurrentArgon2 in-flight logins would.
	for i := 0; i < maxConcurrentArgon2; i++ {
		argonSlots <- struct{}{}
	}
	done := make(chan bool, 1)
	var once sync.Once
	release := func() { once.Do(func() { <-argonSlots }) }
	defer func() {
		release()
		for i := 1; i < maxConcurrentArgon2; i++ {
			<-argonSlots
		}
	}()
	go func() { done <- VerifyPassword(encoded, "bounded") }()

	select {
	case <-done:
		t.Fatal("argon2 ran while every slot was taken")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("password did not verify after a slot freed up")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("verification never acquired the freed slot")
	}
}
