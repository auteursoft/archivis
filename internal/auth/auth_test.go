package auth

import (
	"strings"
	"testing"
	"time"
)

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("encoding %q", h)
	}
	if !VerifyPassword(h, "correct horse battery") {
		t.Fatal("right password refused")
	}
	for _, bad := range []string{"correct horse batterY", "", "correct horse battery "} {
		if VerifyPassword(h, bad) {
			t.Errorf("wrong password %q accepted", bad)
		}
	}
	h2, _ := HashPassword("correct horse battery")
	if h2 == h {
		t.Fatal("two hashes of one password are identical: salt not random")
	}
	// tampered or malformed hashes never match
	parts := strings.Split(h, "$")
	for _, bad := range []string{
		"", "plain", strings.Replace(h, "argon2id", "argon2i", 1),
		strings.Join(append(parts[:5:5], "AAAA"), "$"),
		strings.Replace(h, "m=65536", "m=0", 1),
		strings.Replace(h, "t=3", "t=99", 1),
	} {
		if VerifyPassword(bad, "correct horse battery") {
			t.Errorf("malformed hash %q accepted", bad)
		}
	}
	if VerifyPassword(h, strings.Repeat("x", 300)) {
		t.Error("over-long password accepted")
	}
}

func TestCheckPassword(t *testing.T) {
	for pw, ok := range map[string]bool{
		"short":                  false,
		"elevenchars":            false,
		"twelve chars":           true,
		"Jane Smith-Long":        true,
		strings.Repeat("a", 257): false,
		"ångström-ünicode":       true,
	} {
		if err := CheckPassword("someone", pw); (err == nil) != ok {
			t.Errorf("%q: %v", pw, err)
		}
	}
	if CheckPassword("JaneSmithLong", "janesmithlong") == nil {
		t.Error("password equal to the user name accepted")
	}
}

func TestTokens(t *testing.T) {
	a, ha, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := NewToken()
	if a == b || len(a) < 43 || ha != HashToken(a) || ha == a || strings.ContainsAny(a, "+/=") {
		t.Fatalf("tokens %q %q hash %q", a, b, ha)
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLimiter(3, time.Minute)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if b, _ := l.Blocked("ip"); b {
			t.Fatalf("blocked after %d failures", i)
		}
		l.Fail("ip")
		now = now.Add(time.Second)
	}
	b, wait := l.Blocked("ip")
	if !b || wait <= 0 || wait > time.Minute {
		t.Fatalf("not blocked after 3 failures (wait %v)", wait)
	}
	if b, _ := l.Blocked("other"); b {
		t.Fatal("another key blocked")
	}
	now = now.Add(time.Minute)
	if b, _ := l.Blocked("ip"); b {
		t.Fatal("still blocked after the window")
	}
	l.Fail("ip")
	l.Reset("ip")
	if b, _ := l.Blocked("ip"); b {
		t.Fatal("blocked after reset")
	}
}
