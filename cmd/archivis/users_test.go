package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/auteursoft/archivis/internal/auth"
	"github.com/auteursoft/archivis/internal/store"
)

// answers returns a readPassword that gives these answers in turn.
func answers(a ...string) readPassword {
	return func(string) (string, error) {
		if len(a) == 0 {
			return "", errors.New("no more answers")
		}
		v := a[0]
		a = a[1:]
		return v, nil
	}
}

func TestUsersCommand(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run := func(read readPassword, site string, pos ...string) (string, error) {
		var out bytes.Buffer
		err := usersCmd(st, pos, store.RoleEditor, site, &out, read)
		return out.String(), err
	}
	const pw = "a long enough password"

	if out, err := run(nil, "", "list"); err != nil || !strings.Contains(out, "No accounts yet") {
		t.Fatalf("empty list: %q %v", out, err)
	}
	if _, err := run(nil, "", "invite", "early"); err == nil || !strings.Contains(err.Error(), "add-admin") {
		t.Fatalf("invite before any admin: %v", err)
	}
	if _, err := run(answers("short", "short"), "", "add-admin", "root"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("weak password: %v", err)
	}
	if _, err := run(answers(pw, pw+"!"), "", "add-admin", "root"); err == nil {
		t.Fatal("mismatched passwords accepted")
	}
	if out, err := run(answers(pw, pw), "", "add-admin", "root"); err != nil || !strings.Contains(out, "created admin root") {
		t.Fatalf("add-admin: %q %v", out, err)
	}
	if _, err := run(answers(pw, pw), "", "add-admin", "ROOT"); !errors.Is(err, store.ErrNameTaken) {
		t.Fatalf("duplicate admin: %v", err)
	}
	u, hash, _ := st.UserForLogin("root")
	if u.Role != store.RoleAdmin || !auth.VerifyPassword(hash, pw) {
		t.Fatal("admin not stored with its password")
	}

	out, err := run(nil, "https://photos.example/", "invite", "ann")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`https://photos\.example/invite/([A-Za-z0-9_-]{43})\n`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("invite output: %q", out)
	}
	inv, err := st.InviteByToken(auth.HashToken(m[1]))
	if err != nil || inv.Name != "ann" || inv.Role != store.RoleEditor || inv.ExpiresAt < time.Now().Add(6*24*time.Hour).Unix() {
		t.Fatalf("stored invite: %+v %v", inv, err)
	}
	if out, _ := run(nil, "", "invite", "bob"); !strings.Contains(out, "\n  /invite/") || !strings.Contains(out, "ARCHIVIS_URL") {
		t.Fatalf("invite without a site: %q", out)
	}
	if out, _ := run(nil, "", "list"); !strings.Contains(out, "root") || !strings.Contains(out, "pending invitation: ann (editor)") {
		t.Fatalf("list: %q", out)
	}

	// role, the last-admin guard, disable/enable, sign out
	if _, err := run(nil, "", "role", "root", "viewer"); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("demote last admin: %v", err)
	}
	if _, err := run(nil, "", "disable", "root"); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("disable last admin: %v", err)
	}
	st.CreateSession("s", u.ID, time.Hour, "")
	if out, err := run(nil, "", "signout", "root"); err != nil || !strings.Contains(out, "1 session") {
		t.Fatalf("signout: %q %v", out, err)
	}
	if out, _ := run(nil, "", "signout", "root"); !strings.Contains(out, "no sessions") {
		t.Fatalf("signout again: %q", out)
	}
	hash2, _ := auth.HashPassword(pw)
	st.CreateUser("carl", store.RoleViewer, hash2)
	if _, err := run(nil, "", "role", "carl", "editor"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(nil, "", "role", "carl", "owner"); err == nil {
		t.Fatal("unknown role accepted")
	}
	if out, err := run(nil, "", "disable", "carl"); err != nil || out != "carl disabled\n" {
		t.Fatalf("disable: %q %v", out, err)
	}
	if out, _ := run(nil, "", "list"); !strings.Contains(out, "disabled") {
		t.Fatalf("list after disable: %q", out)
	}
	if _, err := run(nil, "", "reset", "carl"); !errors.Is(err, store.ErrUserDisabled) {
		t.Fatalf("reset for a disabled account: %v", err)
	}
	run(nil, "", "enable", "carl")
	if out, err := run(nil, "x", "reset", "carl"); err != nil || !strings.Contains(out, "x/invite/") {
		t.Fatalf("reset: %q %v", out, err)
	}

	// password: a locked-out admin, from the server's terminal
	st.CreateSession("s2", u.ID, time.Hour, "")
	const pw2 = "a different long password"
	if _, err := run(answers(pw2, pw2), "", "password", "root"); err != nil {
		t.Fatal(err)
	}
	if _, hash, _ := st.UserForLogin("root"); !auth.VerifyPassword(hash, pw2) {
		t.Fatal("password not changed")
	}
	if _, err := st.SessionUser("s2"); err == nil {
		t.Fatal("sessions survived a password change")
	}

	for _, bad := range [][]string{{"role", "nobody", "viewer"}, {"disable"}, {"frobnicate"}, {"password", "nobody"}} {
		if _, err := run(answers(pw, pw), "", bad...); err == nil {
			t.Errorf("users %v: no error", bad)
		}
	}
}
