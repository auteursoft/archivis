package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openUsers(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestRoles(t *testing.T) {
	cases := []struct {
		role, min string
		want      bool
	}{
		{RoleAdmin, RoleViewer, true}, {RoleAdmin, RoleAdmin, true}, {RoleEditor, RoleAdmin, false},
		{RoleViewer, RoleEditor, false}, {RoleEditor, RoleEditor, true}, {"", RoleViewer, false},
		{RoleAdmin, "bogus", false},
	}
	for _, c := range cases {
		if got := RoleAtLeast(c.role, c.min); got != c.want {
			t.Errorf("RoleAtLeast(%q, %q) = %v", c.role, c.min, got)
		}
	}
}

func TestUsersAndLastAdmin(t *testing.T) {
	st := openUsers(t)
	if has, _ := st.HasUsers(); has {
		t.Fatal("fresh store has users")
	}
	admin, err := st.CreateUser("Alice", RoleAdmin, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if has, _ := st.HasUsers(); !has {
		t.Fatal("HasUsers false after CreateUser")
	}
	if _, err := st.CreateUser("alice", RoleViewer, "h"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name (other case): %v", err)
	}
	for _, bad := range []string{"", "  ", "a:b", "a\nb"} {
		if _, err := st.CreateUser(bad, RoleViewer, "h"); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if _, err := st.CreateUser("bob", "root", "h"); err == nil {
		t.Error("unknown role accepted")
	}
	u, hash, err := st.UserForLogin(" ALICE ")
	if err != nil || u.ID != admin.ID || hash != "h1" {
		t.Fatalf("UserForLogin: %v %v %q", u, err, hash)
	}
	if _, _, err := st.UserForLogin("nobody"); !errors.Is(err, ErrNoUser) {
		t.Fatalf("unknown user: %v", err)
	}

	// the only admin can be neither demoted nor disabled
	if err := st.SetRole(admin.ID, RoleEditor); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demote last admin: %v", err)
	}
	if err := st.SetDisabled(admin.ID, true); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disable last admin: %v", err)
	}
	// with a second admin, one may go
	bob, _ := st.CreateUser("bob", RoleAdmin, "h2")
	if err := st.SetRole(admin.ID, RoleEditor); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDisabled(bob.ID, true); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disable remaining admin: %v", err)
	}
	// a disabled admin does not count
	if err := st.SetRole(admin.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDisabled(admin.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRole(bob.ID, RoleViewer); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demote the only active admin: %v", err)
	}
	if err := st.SetDisabled(admin.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRole(999, RoleViewer); !errors.Is(err, ErrNoUser) {
		t.Fatalf("unknown id: %v", err)
	}
	users, err := st.Users()
	if err != nil || len(users) != 2 || users[0].Name != "Alice" || users[1].Name != "bob" {
		t.Fatalf("Users = %+v, %v", users, err)
	}
}

func TestInvites(t *testing.T) {
	st := openUsers(t)
	admin, _ := st.CreateUser("admin", RoleAdmin, "h")

	inv, err := st.CreateInvite("tok1", "carol", RoleEditor, 0, "admin", time.Hour)
	if err != nil || inv.Name != "carol" {
		t.Fatal(inv, err)
	}
	if _, err := st.CreateInvite("tok2", "ADMIN", RoleViewer, 0, "admin", time.Hour); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("invite for an existing name: %v", err)
	}
	if _, err := st.InviteByToken("tok1"); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.PendingInvites(); len(p) != 1 {
		t.Fatalf("pending = %d", len(p))
	}
	u, err := st.AcceptInvite("tok1", "pw")
	if err != nil || u.Name != "carol" || u.Role != RoleEditor {
		t.Fatal(u, err)
	}
	// single use
	if _, err := st.AcceptInvite("tok1", "pw2"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("reused invite: %v", err)
	}
	if _, err := st.InviteByToken("tok1"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("used invite still listed: %v", err)
	}
	if p, _ := st.PendingInvites(); len(p) != 0 {
		t.Fatalf("pending after use = %d", len(p))
	}

	// expired
	if _, err := st.CreateInvite("old", "dave", RoleViewer, 0, "admin", -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcceptInvite("old", "pw"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("expired invite: %v", err)
	}
	// revoked
	st.CreateInvite("rev", "erin", RoleViewer, 0, "admin", time.Hour)
	st.RevokeInvite("rev")
	if _, err := st.AcceptInvite("rev", "pw"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("revoked invite: %v", err)
	}
	// two invites for one name: the second to be accepted fails
	st.CreateInvite("f1", "frank", RoleViewer, 0, "admin", time.Hour)
	st.CreateInvite("f2", "Frank", RoleViewer, 0, "admin", time.Hour)
	if _, err := st.AcceptInvite("f1", "pw"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcceptInvite("f2", "pw"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("second invite for a taken name: %v", err)
	}

	// password reset: replaces the hash and signs out everywhere
	if err := st.CreateSession("s1", admin.ID, time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	reset, err := st.CreateInvite("reset", "", "", admin.ID, "cli", time.Hour)
	if err != nil || reset.Name != "admin" || reset.Role != RoleAdmin {
		t.Fatal(reset, err)
	}
	if u, err := st.AcceptInvite("reset", "newhash"); err != nil || u.ID != admin.ID {
		t.Fatal(u, err)
	}
	if _, h, _ := st.UserForLogin("admin"); h != "newhash" {
		t.Fatalf("hash after reset = %q", h)
	}
	if _, err := st.SessionUser("s1"); !errors.Is(err, ErrNoUser) {
		t.Fatalf("session survived a reset: %v", err)
	}
	// no reset link for a disabled account
	carol, _, _ := st.UserForLogin("carol")
	st.SetDisabled(carol.ID, true)
	if _, err := st.CreateInvite("r2", "", "", carol.ID, "cli", time.Hour); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("reset for disabled: %v", err)
	}
}

func TestSessions(t *testing.T) {
	st := openUsers(t)
	u, _ := st.CreateUser("gina", RoleViewer, "h")
	other, _ := st.CreateUser("admin", RoleAdmin, "h")
	st.CreateSession("a", u.ID, time.Hour, "phone")
	st.CreateSession("b", u.ID, time.Hour, "laptop")
	st.CreateSession("c", other.ID, time.Hour, "")
	st.CreateSession("gone", u.ID, -time.Second, "")

	if got, err := st.SessionUser("a"); err != nil || got.ID != u.ID {
		t.Fatal(got, err)
	}
	if _, err := st.SessionUser("gone"); !errors.Is(err, ErrNoUser) {
		t.Fatalf("expired session: %v", err)
	}
	if _, err := st.SessionUser("nope"); !errors.Is(err, ErrNoUser) {
		t.Fatalf("unknown session: %v", err)
	}
	users, _ := st.Users()
	for _, x := range users {
		if x.ID == u.ID && x.Sessions != 2 {
			t.Fatalf("gina sessions = %d", x.Sessions)
		}
	}
	st.DeleteSession("a")
	if _, err := st.SessionUser("a"); err == nil {
		t.Fatal("signed-out session still valid")
	}
	// disabling signs out everywhere, and re-enabling does not bring sessions back
	if err := st.SetDisabled(u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SessionUser("b"); err == nil {
		t.Fatal("disabled user still signed in")
	}
	st.SetDisabled(u.ID, false)
	if _, err := st.SessionUser("b"); err == nil {
		t.Fatal("session revived by re-enabling")
	}
	if n, err := st.DeleteSessions(other.ID); err != nil || n != 1 {
		t.Fatalf("DeleteSessions = %d, %v", n, err)
	}
	// expired rows are cleared when a new session starts
	var n int
	st.DB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = 'gone'`).Scan(&n)
	if n != 0 {
		t.Fatal("expired session not cleared")
	}
}

func TestNamesUniqueInAnyCase(t *testing.T) {
	st := openUsers(t)
	if _, err := st.CreateUser("Älice", RoleAdmin, "h"); err != nil {
		t.Fatal(err)
	}
	// SQLite's NOCASE would let these through: it folds ASCII only
	for _, dup := range []string{"älice", "ÄLICE", " Älice ", "Älice"} {
		if _, err := st.CreateUser(dup, RoleViewer, "h"); !errors.Is(err, ErrNameTaken) {
			t.Errorf("%q: %v, want ErrNameTaken", dup, err)
		}
		if _, err := st.CreateInvite("t-"+dup, dup, RoleViewer, 0, "x", time.Hour); !errors.Is(err, ErrNameTaken) {
			t.Errorf("invite %q: %v, want ErrNameTaken", dup, err)
		}
	}
	if u, _, err := st.UserForLogin("äLICE"); err != nil || u.Name != "Älice" {
		t.Fatalf("login in another case: %v %v", u, err)
	}
	st.CreateInvite("s1", "straße", RoleViewer, 0, "x", time.Hour)
	st.CreateInvite("s2", "STRASSE", RoleViewer, 0, "x", time.Hour)
	if _, err := st.AcceptInvite("s1", "h"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcceptInvite("s2", "h"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("full case folding: %v", err)
	}
}
