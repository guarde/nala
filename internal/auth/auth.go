package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	pbkdf2Iterations = 600_000
	saltLen          = 16
	keyLen           = 32

	MinPasswordLen = 12
	MaxPasswordLen = 128

	SessionPrefix = "nala_sess_"
	APIKeyPrefix  = "nala_key_"
)

var (
	ErrWeakPassword = fmt.Errorf("password must be %d to %d characters", MinPasswordLen, MaxPasswordLen)
	errBadHash      = errors.New("auth: malformed password hash")
)

func CheckPasswordPolicy(pw string) error {
	n := utf8.RuneCountInString(pw)
	if n < MinPasswordLen || len(pw) > MaxPasswordLen {
		return ErrWeakPassword
	}
	return nil
}

func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iterations, keyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func VerifyPassword(encoded, pw string) bool {
	if len(pw) > MaxPasswordLen {
		return false
	}
	iter, salt, want, err := parseHash(encoded)
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func parseHash(encoded string) (int, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return 0, nil, nil, errBadHash
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return 0, nil, nil, errBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return 0, nil, nil, errBadHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(key) == 0 {
		return 0, nil, nil, errBadHash
	}
	return iter, salt, key, nil
}

var dummyHash = func() string {
	h, err := HashPassword("nala-is-a-stinky-girl")
	if err != nil {
		panic(err)
	}
	return h
}()

func BurnPasswordCheck(pw string) { VerifyPassword(dummyHash, pw) }

func NewToken(prefix string) (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", nil, err
	}
	token = prefix + base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}
