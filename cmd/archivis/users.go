package main

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/auteursoft/archivis/internal/auth"
	"github.com/auteursoft/archivis/internal/catalog"
	"github.com/auteursoft/archivis/internal/store"
	"github.com/auteursoft/archivis/internal/web"
)

const usersHelp = `Manage web accounts. Once the first account exists, everyone signs in
with their own name and password, and --auth / ARCHIVIS_AUTH is no longer used.
  users list
  users add-admin NAME          create an admin (asks for a password)
  users invite NAME [--role R]  print a single-use invitation link (7 days)
  users reset NAME              print a link to choose a new password
  users password NAME           set a password here (e.g. a locked-out admin)
  users role NAME ROLE          viewer | editor | admin
  users disable NAME            block sign-in and end all sessions
  users enable NAME
  users signout NAME            end all sessions
Roles: viewer browses and searches; editor also names people, rates photos and
downloads originals; admin also manages accounts (in the web interface too).`

func runUsers(g *globals, args []string) error {
	fs := newFlagSet("users", usersHelp)
	role := fs.String("role", store.RoleViewer, "role for invite: viewer, editor or admin")
	site := fs.String("url", os.Getenv("ARCHIVIS_URL"), "the site's address for links, e.g. https://photos.example.com (default $ARCHIVIS_URL)")
	g.register(fs)
	pos := parseInterleaved(fs, args)
	if len(pos) == 0 {
		pos = []string{"list"}
	}
	cat, err := catalog.Open(g.data, nil)
	if err != nil {
		return err
	}
	defer cat.Close()
	return usersCmd(cat.Store, pos, *role, *site, os.Stdout, terminalPassword)
}

// readPassword asks for a password (prompt says what for).
type readPassword func(prompt string) (string, error)

// terminalPassword reads a password without echoing it, or one line from
// standard input when that is not a terminal (for scripts).
func terminalPassword(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			return "", fmt.Errorf("reading a password from standard input: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

// newPassword asks for a password twice and checks it.
func newPassword(name string, read readPassword) (string, error) {
	pw, err := read(fmt.Sprintf("New password for %s (at least %d characters): ", name, auth.MinPasswordLen))
	if err != nil {
		return "", err
	}
	if err := auth.CheckPassword(name, pw); err != nil {
		return "", err
	}
	again, err := read("Again: ")
	if err != nil {
		return "", err
	}
	if again != pw {
		return "", errors.New("the two passwords differ")
	}
	return auth.HashPassword(pw)
}

func usersCmd(st *store.Store, pos []string, role, site string, out io.Writer, read readPassword) error {
	want := func(n int, usage string) error {
		if len(pos) != n {
			return fmt.Errorf("usage: archivis users %s", usage)
		}
		return nil
	}
	lookup := func(name string) (*store.User, error) {
		u, _, err := st.UserForLogin(name)
		if errors.Is(err, store.ErrNoUser) {
			return nil, fmt.Errorf("no account named %q", name)
		}
		return u, err
	}
	link := func(token string) string {
		return strings.TrimRight(site, "/") + "/invite/" + token
	}
	printLink := func(what, name, token string) {
		fmt.Fprintf(out, "%s for %s (works once, until %s):\n\n  %s\n\n", what, name,
			time.Now().Add(web.InviteTTL).Format("2 Jan 15:04"), link(token))
		if site == "" {
			fmt.Fprintln(out, "Put the site's address in front, e.g. https://photos.example.com/invite/…, or pass --url / set ARCHIVIS_URL.")
		}
	}
	switch pos[0] {
	case "list":
		if err := want(1, "list"); err != nil {
			return err
		}
		users, err := st.Users()
		if err != nil {
			return err
		}
		if len(users) == 0 {
			fmt.Fprintln(out, "No accounts yet: the web interface uses --auth (or nothing). Create one with: archivis users add-admin NAME")
			return nil
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tROLE\tSTATUS\tSIGNED IN ON\tCREATED")
		for _, u := range users {
			status := "active"
			if u.Disabled {
				status = "disabled"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", u.Name, u.Role, status, u.Sessions, time.Unix(u.CreatedAt, 0).Format("2006-01-02"))
		}
		tw.Flush()
		invites, err := st.PendingInvites()
		if err != nil {
			return err
		}
		for _, inv := range invites {
			kind := "invitation"
			if inv.UserID != 0 {
				kind = "password reset"
			}
			fmt.Fprintf(out, "pending %s: %s (%s), until %s\n", kind, inv.Name, inv.Role, time.Unix(inv.ExpiresAt, 0).Format("2 Jan 15:04"))
		}
		return nil
	case "add-admin":
		if err := want(2, "add-admin NAME"); err != nil {
			return err
		}
		if _, err := lookup(pos[1]); err == nil {
			return store.ErrNameTaken
		}
		hash, err := newPassword(strings.TrimSpace(pos[1]), read)
		if err != nil {
			return err
		}
		u, err := st.CreateUser(pos[1], store.RoleAdmin, hash)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "created admin %s; the web interface now asks everyone to sign in\n", u.Name)
		return nil
	case "invite":
		if err := want(2, "invite NAME [--role viewer|editor|admin]"); err != nil {
			return err
		}
		if has, err := st.HasUsers(); err != nil {
			return err
		} else if !has {
			return errors.New("create your own account first: archivis users add-admin NAME")
		}
		tok, hash, err := auth.NewToken()
		if err != nil {
			return err
		}
		inv, err := st.CreateInvite(hash, pos[1], role, 0, "command line", web.InviteTTL)
		if err != nil {
			return err
		}
		printLink("Invitation ("+inv.Role+")", inv.Name, tok)
		return nil
	case "reset":
		if err := want(2, "reset NAME"); err != nil {
			return err
		}
		u, err := lookup(pos[1])
		if err != nil {
			return err
		}
		tok, hash, err := auth.NewToken()
		if err != nil {
			return err
		}
		if _, err := st.CreateInvite(hash, "", "", u.ID, "command line", web.InviteTTL); err != nil {
			return err
		}
		printLink("Password-reset link", u.Name, tok)
		return nil
	case "password":
		if err := want(2, "password NAME"); err != nil {
			return err
		}
		u, err := lookup(pos[1])
		if err != nil {
			return err
		}
		hash, err := newPassword(u.Name, read)
		if err != nil {
			return err
		}
		if err := st.SetPassword(u.ID, hash); err != nil {
			return err
		}
		fmt.Fprintf(out, "password set for %s; their other sessions have ended\n", u.Name)
		return nil
	case "role":
		if err := want(3, "role NAME viewer|editor|admin"); err != nil {
			return err
		}
		u, err := lookup(pos[1])
		if err != nil {
			return err
		}
		if err := st.SetRole(u.ID, pos[2]); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s is now %s\n", u.Name, pos[2])
		return nil
	case "disable", "enable":
		if err := want(2, pos[0]+" NAME"); err != nil {
			return err
		}
		u, err := lookup(pos[1])
		if err != nil {
			return err
		}
		if err := st.SetDisabled(u.ID, pos[0] == "disable"); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s %sd\n", u.Name, pos[0])
		return nil
	case "signout":
		if err := want(2, "signout NAME"); err != nil {
			return err
		}
		u, err := lookup(pos[1])
		if err != nil {
			return err
		}
		n, err := st.DeleteSessions(u.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s signed out of %s\n", u.Name, cmp.Or(plural(int(n), "session"), "no sessions"))
		return nil
	}
	return fmt.Errorf("unknown users command %q; run archivis users -h", pos[0])
}

func plural(n int, word string) string {
	switch n {
	case 0:
		return ""
	case 1:
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
