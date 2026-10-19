// Package auth holds the platform's credential primitives: argon2id password hashing and
// HS256 JWTs. Both are deliberately small and dependency-light (only golang.org/x/crypto for
// argon2), consistent with the repository's minimal-dependency philosophy (ADR 0012).
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters. These are the cost knobs; they are encoded into every hash so a future
// change to them does not invalidate existing passwords (the stored parameters are used for
// verification, the current ones only for new hashes).
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword returns a PHC-style encoded argon2id hash:
//
//	$argon2id$v=19$m=65536,t=1,p=4$<b64salt>$<b64hash>
//
// Password plaintext is never stored; only this string is (ADR 0014).
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// VerifyPassword reports whether password matches the encoded argon2id hash. It returns false
// for any malformed encoding rather than erroring, so callers have a single boolean to branch
// on. The comparison is constant-time.
func VerifyPassword(encoded, password string) bool {
	salt, want, memory, time, threads, keyLen, err := decodeArgon2(encoded)
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, threads, keyLen)
	return subtle.ConstantTimeCompare(got, want) == 1
}

func decodeArgon2(encoded string) (salt, hash []byte, memory uint32, time uint32, threads uint8, keyLen uint32, err error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", "<salt>", "<hash>"]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return nil, nil, 0, 0, 0, 0, errors.New("bad argon2 encoding")
	}
	var version int
	if _, err = fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return nil, nil, 0, 0, 0, 0, errors.New("bad argon2 version")
	}
	if _, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return nil, nil, 0, 0, 0, 0, errors.New("bad argon2 params")
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return nil, nil, 0, 0, 0, 0, errors.New("bad argon2 salt")
	}
	if hash, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return nil, nil, 0, 0, 0, 0, errors.New("bad argon2 hash")
	}
	return salt, hash, memory, time, threads, uint32(len(hash)), nil
}
