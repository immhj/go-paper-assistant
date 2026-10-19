package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// JWT issues and verifies HS256 tokens for platform accounts. Revocation is enforced by the
// service comparing Claims.AuthVersion with the current account row on every authenticated
// request.
type JWT struct {
	secret []byte
	ttl    time.Duration
}

func NewJWT(secret string, ttl time.Duration) *JWT {
	return &JWT{secret: []byte(secret), ttl: ttl}
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// Claims is the authenticated payload carried by a platform JWT. Username is serialized as the
// conventional subject claim; AuthVersion makes password resets and account status changes revoke
// previously issued tokens without a server-side token blacklist.
type Claims struct {
	Username    string `json:"sub"`
	AuthVersion uint64 `json:"auth_version"`
	Exp         int64  `json:"exp"`
	Iat         int64  `json:"iat"`
}

// Issue returns a signed token for username and its current durable authentication version.
func (j *JWT) Issue(username string, authVersion uint64) (string, error) {
	if strings.TrimSpace(username) == "" {
		return "", errors.New("username is required")
	}
	if authVersion == 0 {
		return "", errors.New("auth version is required")
	}
	now := time.Now()
	header, err := json.Marshal(jwtHeader{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("marshal jwt header: %w", err)
	}
	claims, err := json.Marshal(Claims{
		Username: username, AuthVersion: authVersion, Exp: now.Add(j.ttl).Unix(), Iat: now.Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("marshal jwt claims: %w", err)
	}
	signingInput := encodeSegment(header) + "." + encodeSegment(claims)
	return signingInput + "." + encodeSegment(j.sign(signingInput)), nil
}

// Verify validates structure, the fixed HS256 algorithm, signature, required claims and expiry.
func (j *JWT) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("malformed token")
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, errors.New("bad header encoding")
	}
	var header jwtHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil || header.Alg != "HS256" || header.Typ != "JWT" {
		return Claims{}, errors.New("unsupported jwt header")
	}

	signingInput := parts[0] + "." + parts[1]
	wantSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, errors.New("bad signature encoding")
	}
	if !hmac.Equal(wantSig, j.sign(signingInput)) {
		return Claims{}, errors.New("signature mismatch")
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, errors.New("bad claims encoding")
	}
	var claims Claims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return Claims{}, errors.New("bad claims json")
	}
	now := time.Now().Unix()
	if claims.Username == "" || claims.AuthVersion == 0 || claims.Exp == 0 || claims.Iat == 0 {
		return Claims{}, errors.New("missing required claims")
	}
	if now >= claims.Exp {
		return Claims{}, errors.New("token expired")
	}
	if claims.Iat > now+60 {
		return Claims{}, errors.New("token issued in the future")
	}
	return claims, nil
}

func (j *JWT) sign(input string) []byte {
	mac := hmac.New(sha256.New, j.secret)
	_, _ = mac.Write([]byte(input))
	return mac.Sum(nil)
}

func encodeSegment(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
