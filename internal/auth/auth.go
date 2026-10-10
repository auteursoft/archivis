// Package auth holds the security primitives behind Archivis accounts:
// password hashing (Argon2id), random tokens stored only as hashes, and a
// limiter that slows down password guessing.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Argon2id parameters (RFC 9106's second recommended option, with 64 MiB):
// about 50-100 ms per hash on a current machine, which makes each guess
// against a stolen database expensive.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16
)

// MinPasswordLen is the shortest password accepted.
const MinPasswordLen = 12

// maxPasswordLen is the longest password accepted, in characters;
// maxPasswordBytes bounds the hashing work an attacker can ask for.
const (
	maxPasswordLen   = 256
	maxPasswordBytes = 4 * maxPasswordLen
)

// ErrWeakPassword describes why a new password was refused.
var ErrWeakPassword = errors.New("password too weak")

// CheckPassword reports whether a new password is acceptable for user name.
func CheckPassword(name, pw string) error {
	n := utf8.RuneCountInString(pw)
	switch {
	case n < MinPasswordLen:
		return fmt.Errorf("%w: use at least %d characters", ErrWeakPassword, MinPasswordLen)
	case n > maxPasswordLen || len(pw) > maxPasswordBytes:
		return fmt.Errorf("%w: use at most %d characters", ErrWeakPassword, maxPasswordLen)
	case NameKey(pw) == NameKey(name):
		return fmt.Errorf("%w: it must differ from the user name", ErrWeakPassword)
	}
	return nil
}

// NameKey is the form account names are compared in: Unicode-normalised
// and case-folded, so "Älice" and "älice", or "straße" and "STRASSE", are
// one name.
func NameKey(name string) string {
	return cases.Fold().String(norm.NFKC.String(strings.TrimSpace(name)))
}

// HashPassword returns an encoded Argon2id hash with a fresh random salt:
// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether pw matches an encoded hash. The comparison
// takes constant time, and a malformed hash never matches.
func VerifyPassword(encoded, pw string) bool {
	if len(pw) > maxPasswordBytes {
		return false
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m == 0 || m > 1<<22 || t == 0 || t > 16 || p == 0 {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err1 := b64.DecodeString(parts[4])
	want, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(salt) < 8 || len(want) < 16 || len(want) > 64 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is verified against when a login names no account, so an
// unknown user takes as long to refuse as a wrong password and the timing
// does not reveal which names exist.
var dummyHash = func() string {
	h, err := HashPassword("not-a-real-password-for-timing")
	if err != nil {
		panic(err)
	}
	return h
}()

// BurnTime does the same work as a password check, for unknown users.
func BurnTime(pw string) { VerifyPassword(dummyHash, pw) }

// NewToken returns a random URL-safe token (256 bits) and its hash, which is
// what gets stored: a copy of the database cannot be used to sign in or to
// accept an invite.
func NewToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

// HashToken is the stored form of a token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Limiter counts failed attempts per key (a client address, a user name)
// in a sliding window, and refuses further attempts once a key reaches the
// limit, until its oldest failure leaves the window.
type Limiter struct {
	Max    int
	Window time.Duration
	now    func() time.Time

	mu    sync.Mutex
	fails map[string][]time.Time
}

// maxLimiterKeys bounds a Limiter's memory (about 100 bytes a key).
var maxLimiterKeys = 100_000

// NewLimiter allows max failures per key per window.
func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{Max: max, Window: window, now: time.Now, fails: map[string][]time.Time{}}
}

// Blocked reports whether key must wait, and for how long.
func (l *Limiter) Blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.prune(key)
	if len(f) < l.Max {
		return false, 0
	}
	return true, f[0].Add(l.Window).Sub(l.now())
}

// Fail records a failed attempt for key.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[key] = append(l.prune(key), l.now())
	if len(l.fails) > maxLimiterKeys { // bound memory under a flood of distinct keys
		for k := range l.fails {
			if len(l.prune(k)) == 0 {
				delete(l.fails, k)
			}
		}
		// Still full of active keys: forget arbitrary others (map order is
		// random) rather than grow. Only a flood from thousands of addresses
		// gets here, and it then loses some of its own counts.
		for k := range l.fails {
			if len(l.fails) <= maxLimiterKeys {
				break
			}
			if k != key {
				delete(l.fails, k)
			}
		}
	}
}

// Reset forgets key's failures (after a successful sign-in).
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

func (l *Limiter) prune(key string) []time.Time {
	cut := l.now().Add(-l.Window)
	f := l.fails[key]
	i := 0
	for i < len(f) && !f[i].After(cut) {
		i++
	}
	f = f[i:]
	if len(f) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = f
	return f
}
