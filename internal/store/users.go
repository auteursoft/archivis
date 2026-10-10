package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Roles, from least to most able.
const (
	RoleViewer = "viewer" // browse and search
	RoleEditor = "editor" // also name people, fix labels, rate photos, download originals
	RoleAdmin  = "admin"  // also invite people and manage accounts
)

// ValidRole reports whether r is a known role.
func ValidRole(r string) bool { return r == RoleViewer || r == RoleEditor || r == RoleAdmin }

// RoleAtLeast reports whether role has at least the rights of min.
func RoleAtLeast(role, min string) bool {
	rank := map[string]int{RoleViewer: 1, RoleEditor: 2, RoleAdmin: 3}
	return rank[role] >= rank[min] && rank[min] > 0
}

var (
	ErrNameTaken     = errors.New("that name is already in use")
	ErrNoUser        = errors.New("no such user")
	ErrInviteInvalid = errors.New("this invite link is invalid, used or expired")
	ErrLastAdmin     = errors.New("there must always be at least one active admin")
	ErrUserDisabled  = errors.New("this account is disabled")
)

// User is an account.
type User struct {
	ID        int64
	Name      string
	Role      string
	CreatedAt int64
	Disabled  bool
	Sessions  int // signed-in devices (Users only)
}

const userCols = `u.id, u.name, u.role, u.created_at, u.disabled_at IS NOT NULL`

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	var u User
	if err := sc.Scan(&u.ID, &u.Name, &u.Role, &u.CreatedAt, &u.Disabled); err != nil {
		return nil, err
	}
	return &u, nil
}

// cleanName validates an account name.
func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 || strings.ContainsAny(name, ":\x00\r\n\t") {
		return "", fmt.Errorf("names must be 1-64 characters, without ':' or control characters")
	}
	return name, nil
}

// NameKey is the form names are compared in: Unicode-normalised and
// case-folded, so "Älice" and "älice" are one account (SQLite's NOCASE
// folds ASCII only).
func NameKey(name string) string {
	return cases.Fold().String(norm.NFKC.String(strings.TrimSpace(name)))
}

// HasUsers reports whether any account exists. Until one does, the web
// interface uses the older single shared password (or none).
func (s *Store) HasUsers() (bool, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n > 0, err
}

// CreateUser adds an account with a password hash (from the command line;
// people invited through the web set their own).
func (s *Store) CreateUser(name, role, pwHash string) (*User, error) {
	name, err := cleanName(name)
	if err != nil {
		return nil, err
	}
	if !ValidRole(role) {
		return nil, fmt.Errorf("unknown role %q", role)
	}
	res, err := s.DB.Exec(`INSERT INTO users (name, name_key, role, pw_hash, created_at) VALUES (?,?,?,?,?)`, name, NameKey(name), role, pwHash, time.Now().Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrNameTaken
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.UserByID(id)
}

// UserByID returns an account.
func (s *Store) UserByID(id int64) (*User, error) {
	u, err := scanUser(s.DB.QueryRow(`SELECT `+userCols+` FROM users u WHERE u.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoUser
	}
	return u, err
}

// UserForLogin returns an account by name (any letter case) and its
// password hash ("" if it has none yet).
func (s *Store) UserForLogin(name string) (*User, string, error) {
	var hash sql.NullString
	var u User
	err := s.DB.QueryRow(`SELECT `+userCols+`, u.pw_hash FROM users u WHERE u.name_key = ?`, NameKey(name)).
		Scan(&u.ID, &u.Name, &u.Role, &u.CreatedAt, &u.Disabled, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNoUser
	}
	if err != nil {
		return nil, "", err
	}
	return &u, hash.String, nil
}

// Users lists accounts by name, with how many devices each is signed in on.
func (s *Store) Users() ([]User, error) {
	rows, err := s.DB.Query(`SELECT ` + userCols + `,
		(SELECT COUNT(*) FROM sessions x WHERE x.user_id = u.id AND x.expires_at > strftime('%s','now'))
		FROM users u ORDER BY u.name_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.Role, &u.CreatedAt, &u.Disabled, &u.Sessions); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// otherActiveAdmins counts active admins other than id, inside tx.
func otherActiveAdmins(tx *sql.Tx, id int64) (int, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ? AND disabled_at IS NULL AND id != ?`, RoleAdmin, id).Scan(&n)
	return n, err
}

// SetRole changes an account's role; the last active admin cannot be
// demoted.
func (s *Store) SetRole(id int64, role string) error {
	if !ValidRole(role) {
		return fmt.Errorf("unknown role %q", role)
	}
	return s.inTx(func(tx *sql.Tx) error {
		var cur string
		var disabled bool
		if err := tx.QueryRow(`SELECT role, disabled_at IS NOT NULL FROM users WHERE id = ?`, id).Scan(&cur, &disabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNoUser
			}
			return err
		}
		if cur == RoleAdmin && role != RoleAdmin && !disabled {
			if n, err := otherActiveAdmins(tx, id); err != nil {
				return err
			} else if n == 0 {
				return ErrLastAdmin
			}
		}
		_, err := tx.Exec(`UPDATE users SET role = ? WHERE id = ?`, role, id)
		return err
	})
}

// SetDisabled disables (signing the account out everywhere) or re-enables
// an account; the last active admin cannot be disabled.
func (s *Store) SetDisabled(id int64, disabled bool) error {
	return s.inTx(func(tx *sql.Tx) error {
		var role string
		if err := tx.QueryRow(`SELECT role FROM users WHERE id = ?`, id).Scan(&role); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNoUser
			}
			return err
		}
		if !disabled {
			_, err := tx.Exec(`UPDATE users SET disabled_at = NULL WHERE id = ?`, id)
			return err
		}
		if role == RoleAdmin {
			if n, err := otherActiveAdmins(tx, id); err != nil {
				return err
			} else if n == 0 {
				return ErrLastAdmin
			}
		}
		if _, err := tx.Exec(`UPDATE users SET disabled_at = ? WHERE id = ? AND disabled_at IS NULL`, time.Now().Unix(), id); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)
		return err
	})
}

// Invite is a pending invitation (or password reset, when UserID is set).
type Invite struct {
	Name, Role, CreatedBy string
	UserID                int64
	CreatedAt, ExpiresAt  int64
	TokenHash             string
}

// CreateInvite stores an invitation under the hash of its token. For a new
// account the name must be free; for a password reset (userID != 0) it is
// that account's name.
func (s *Store) CreateInvite(tokenHash, name, role string, userID int64, createdBy string, ttl time.Duration) (*Invite, error) {
	inv := &Invite{Role: role, UserID: userID, CreatedBy: createdBy, TokenHash: tokenHash,
		CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(ttl).Unix()}
	if userID != 0 {
		u, err := s.UserByID(userID)
		if err != nil {
			return nil, err
		}
		if u.Disabled {
			return nil, ErrUserDisabled
		}
		inv.Name, inv.Role = u.Name, u.Role
	} else {
		var err error
		if inv.Name, err = cleanName(name); err != nil {
			return nil, err
		}
		if !ValidRole(role) {
			return nil, fmt.Errorf("unknown role %q", role)
		}
		if _, _, err := s.UserForLogin(inv.Name); err == nil {
			return nil, ErrNameTaken
		}
	}
	uid := sql.NullInt64{Int64: userID, Valid: userID != 0}
	_, err := s.DB.Exec(`INSERT INTO invites (token_hash, name, role, user_id, created_by, created_at, expires_at) VALUES (?,?,?,?,?,?,?)`,
		tokenHash, inv.Name, inv.Role, uid, createdBy, inv.CreatedAt, inv.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// InviteByToken returns a usable invitation (unused and unexpired).
func (s *Store) InviteByToken(tokenHash string) (*Invite, error) {
	var inv Invite
	var uid sql.NullInt64
	var by sql.NullString
	err := s.DB.QueryRow(`SELECT name, role, user_id, created_by, created_at, expires_at, token_hash FROM invites
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`, tokenHash, time.Now().Unix()).
		Scan(&inv.Name, &inv.Role, &uid, &by, &inv.CreatedAt, &inv.ExpiresAt, &inv.TokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInviteInvalid
	}
	inv.UserID, inv.CreatedBy = uid.Int64, by.String
	return &inv, err
}

// PendingInvites lists unused, unexpired invitations, newest first.
func (s *Store) PendingInvites() ([]Invite, error) {
	rows, err := s.DB.Query(`SELECT name, role, user_id, created_by, created_at, expires_at, token_hash FROM invites
		WHERE used_at IS NULL AND expires_at > ? ORDER BY created_at DESC`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var inv Invite
		var uid sql.NullInt64
		var by sql.NullString
		if err := rows.Scan(&inv.Name, &inv.Role, &uid, &by, &inv.CreatedAt, &inv.ExpiresAt, &inv.TokenHash); err != nil {
			return nil, err
		}
		inv.UserID, inv.CreatedBy = uid.Int64, by.String
		out = append(out, inv)
	}
	return out, rows.Err()
}

// RevokeInvite cancels an invitation.
func (s *Store) RevokeInvite(tokenHash string) error {
	_, err := s.DB.Exec(`DELETE FROM invites WHERE token_hash = ? AND used_at IS NULL`, tokenHash)
	return err
}

// AcceptInvite uses an invitation once: it creates the account (or, for a
// reset, replaces the password and signs the account out everywhere) and
// marks the invitation used, in one transaction.
func (s *Store) AcceptInvite(tokenHash, pwHash string) (*User, error) {
	var id int64
	err := s.inTx(func(tx *sql.Tx) error {
		now := time.Now().Unix()
		var name, role string
		var uid sql.NullInt64
		err := tx.QueryRow(`SELECT name, role, user_id FROM invites WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
			tokenHash, now).Scan(&name, &role, &uid)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInviteInvalid
		}
		if err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE invites SET used_at = ? WHERE token_hash = ? AND used_at IS NULL`, now, tokenHash)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrInviteInvalid
		}
		if uid.Valid {
			var disabled bool
			if err := tx.QueryRow(`SELECT disabled_at IS NOT NULL FROM users WHERE id = ?`, uid.Int64).Scan(&disabled); err != nil {
				return ErrInviteInvalid
			}
			if disabled {
				return ErrUserDisabled
			}
			if _, err := tx.Exec(`UPDATE users SET pw_hash = ? WHERE id = ?`, pwHash, uid.Int64); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, uid.Int64); err != nil {
				return err
			}
			id = uid.Int64
			return nil
		}
		res, err = tx.Exec(`INSERT INTO users (name, name_key, role, pw_hash, created_at) VALUES (?,?,?,?,?)`, name, NameKey(name), role, pwHash, now)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return ErrNameTaken
			}
			return err
		}
		id, _ = res.LastInsertId()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.UserByID(id)
}

// ErrStaleSignIn means the password changed, or the account was disabled,
// while a sign-in was being checked.
var ErrStaleSignIn = errors.New("the password changed during sign-in; sign in again")

// CreateSession records a signed-in device under the hash of its token, if
// the account is still enabled and its password hash is still pwHash: a
// sign-in checked against the old password while a reset or disable
// committed does not survive it.
func (s *Store) CreateSession(tokenHash string, userID int64, pwHash string, ttl time.Duration, client string) error {
	now := time.Now()
	if len(client) > 200 {
		client = client[:200]
	}
	// expired sessions are cleared here, at a natural quiet moment
	if _, err := s.DB.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, now.Unix()); err != nil {
		return err
	}
	res, err := s.DB.Exec(`INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_seen, client)
		SELECT ?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM users WHERE id = ? AND pw_hash = ? AND disabled_at IS NULL)`,
		tokenHash, userID, now.Unix(), now.Add(ttl).Unix(), now.Unix(), client, userID, pwHash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleSignIn
	}
	return nil
}

// SessionUser returns the account signed in with a session, if the session
// is current and the account enabled. Its last-seen time is updated at most
// once an hour.
func (s *Store) SessionUser(tokenHash string) (*User, error) {
	now := time.Now().Unix()
	var lastSeen int64
	var u User
	err := s.DB.QueryRow(`SELECT `+userCols+`, x.last_seen FROM sessions x JOIN users u ON u.id = x.user_id
		WHERE x.token_hash = ? AND x.expires_at > ? AND u.disabled_at IS NULL`, tokenHash, now).
		Scan(&u.ID, &u.Name, &u.Role, &u.CreatedAt, &u.Disabled, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoUser
	}
	if err != nil {
		return nil, err
	}
	if now-lastSeen > 3600 {
		s.DB.Exec(`UPDATE sessions SET last_seen = ? WHERE token_hash = ?`, now, tokenHash)
	}
	return &u, nil
}

// DeleteSession signs one device out.
func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteSessions signs an account out everywhere.
func (s *Store) DeleteSessions(userID int64) (int64, error) {
	res, err := s.DB.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) inTx(fn func(*sql.Tx) error) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// SetPassword replaces an account's password and signs it out everywhere.
func (s *Store) SetPassword(id int64, pwHash string) error {
	return s.inTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE users SET pw_hash = ? WHERE id = ?`, pwHash, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNoUser
		}
		_, err = tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)
		return err
	})
}
