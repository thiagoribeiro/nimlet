package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/thiagoribeiro/nimlet/internal/cursor"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
)

type userJSON struct {
	ID          string `json:"id"`
	NamespaceID string `json:"namespace_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
	Disabled    bool   `json:"disabled"`
	CreatedAt   string `json:"created_at"`
}

func userRow(u *domain.User) row {
	j := userJSON{ID: u.ID, NamespaceID: u.NamespaceID, Username: u.Username, DisplayName: u.DisplayName,
		Type: string(u.Type), Disabled: u.Disabled, CreatedAt: ts(u.CreatedAt)}
	return row{id: u.ID, json: j, fields: [][2]string{
		{"username", u.Username}, {"id", u.ID}, {"display name", u.DisplayName}, {"type", j.Type},
		{"disabled", strconv.FormatBool(u.Disabled)}, {"created", j.CreatedAt},
	}}
}

type sessionJSON struct {
	ID        string `json:"id"`
	DeviceID  string `json:"device_id"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

func sessionRow(s *domain.Session) row {
	j := sessionJSON{ID: s.ID, DeviceID: s.DeviceID, CreatedAt: ts(s.CreatedAt), ExpiresAt: ts(s.ExpiresAt)}
	return row{id: s.ID, json: j, fields: [][2]string{
		{"session", s.ID}, {"device", s.DeviceID}, {"created", j.CreatedAt}, {"expires", j.ExpiresAt},
	}}
}

func newUserCmd(f *adminFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage users of a namespace",
	}
	addNamespaceFlag(cmd, f)

	var displayName, userType string
	var passwordStdin bool
	create := &cobra.Command{
		Use:   "create <username>",
		Short: "Create a user (password from a prompt or --password-stdin)",
		Example: "  nimlet user create alice -n acme --display-name Alice\n" +
			"  printf '%s\\n' \"$PW\" | nimlet user create bot -n acme --type bot --password-stdin",
		Args: args(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, a []string) error {
			return f.run(c, func(svc in.AdminService, ns string, mode outputMode) error {
				pw, err := readPassword(c, passwordStdin)
				if err != nil {
					return err
				}
				u, err := svc.CreateUser(c.Context(), in.AdminCreateUserCmd{
					Namespace: ns, Username: a[0], DisplayName: displayName,
					Type: domain.UserType(userType), Password: pw,
				})
				if err != nil {
					return err
				}
				return printRows(c.OutOrStdout(), mode, []row{userRow(u)}, false)
			})
		},
	}
	create.Flags().StringVar(&displayName, "display-name", "", "display name (default: the username)")
	create.Flags().StringVar(&userType, "type", "human", "human, agent, bot, service or system")
	create.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from the first line of stdin")

	var limit int
	var after string
	list := &cobra.Command{
		Use:   "list",
		Short: "List users, by username",
		Args:  args(cobra.NoArgs),
		RunE: func(c *cobra.Command, _ []string) error {
			return f.run(c, func(svc in.AdminService, ns string, mode outputMode) error {
				us, err := svc.ListUsers(c.Context(), ns, after, limit)
				if err != nil {
					return err
				}
				rows := make([]row, len(us))
				for i, u := range us {
					rows[i] = userRow(u)
				}
				return printRows(c.OutOrStdout(), mode, rows, true)
			})
		},
	}
	list.Flags().IntVar(&limit, "limit", 100, "maximum users to list")
	list.Flags().StringVar(&after, "after", "", "list users after this username (pagination)")

	show := &cobra.Command{
		Use:   "show <username>",
		Short: "Show a user",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, a []string) error {
			return f.run(c, func(svc in.AdminService, ns string, mode outputMode) error {
				u, err := svc.GetUser(c.Context(), ns, a[0])
				if err != nil {
					return err
				}
				return printRows(c.OutOrStdout(), mode, []row{userRow(u)}, false)
			})
		},
	}

	var at int64
	syncCursor := &cobra.Command{
		Use:   "sync-cursor <username>",
		Short: "Issue the user's /sync cursor for a stream position",
		Long: "Prints the opaque /sync cursor (next_batch) that resumes after stream position --at.\n" +
			"For consumers that kept a numeric cursor from before opaque cursors: convert it once\n" +
			"instead of letting them replay the whole stream. Needs the server's\n" +
			"NIMLET_AUTH_HMAC_SECRET.",
		Example: "  NIMLET_AUTH_HMAC_SECRET=... nimlet user sync-cursor assistant-bot -n default --at 1234",
		Args:    args(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, a []string) error {
			if !c.Flags().Changed("at") || at < 0 {
				return usageError{errors.New("--at <stream position> (>= 0) is required")}
			}
			secret := os.Getenv("NIMLET_AUTH_HMAC_SECRET")
			if len(secret) < 32 {
				return usageError{errors.New("NIMLET_AUTH_HMAC_SECRET (the server's, at least 32 characters) is required")}
			}
			codec, err := cursor.New([]byte(secret))
			if err != nil {
				return err
			}
			return f.run(c, func(svc in.AdminService, ns string, _ outputMode) error {
				u, err := svc.GetUser(c.Context(), ns, a[0])
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(c.OutOrStdout(), codec.Encode(cursor.Sync, u.ID, at))
				return err
			})
		},
	}
	syncCursor.Flags().Int64Var(&at, "at", 0, "stream position the cursor resumes after: the old numeric next_batch (required)")

	setDisabled := func(disabled bool) func(*cobra.Command, []string) error {
		return func(c *cobra.Command, a []string) error {
			return f.run(c, func(svc in.AdminService, ns string, mode outputMode) error {
				if err := svc.SetUserDisabled(c.Context(), ns, a[0], disabled); err != nil {
					return err
				}
				u, err := svc.GetUser(c.Context(), ns, a[0])
				if err != nil {
					return err
				}
				return printRows(c.OutOrStdout(), mode, []row{userRow(u)}, false)
			})
		}
	}
	disable := &cobra.Command{
		Use:   "disable <username>",
		Short: "Disable a user: no login, and existing sessions stop working at once",
		Args:  args(cobra.ExactArgs(1)),
		RunE:  setDisabled(true),
	}
	enable := &cobra.Command{
		Use:   "enable <username>",
		Short: "Re-enable a user",
		Args:  args(cobra.ExactArgs(1)),
		RunE:  setDisabled(false),
	}

	password := &cobra.Command{Use: "password", Short: "Manage a user's password"}
	var setStdin bool
	passwordSet := &cobra.Command{
		Use:   "set <username>",
		Short: "Set a user's password (prompt or --password-stdin)",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, a []string) error {
			return f.run(c, func(svc in.AdminService, ns string, _ outputMode) error {
				pw, err := readPassword(c, setStdin)
				if err != nil {
					return err
				}
				return svc.SetUserPassword(c.Context(), ns, a[0], pw)
			})
		},
	}
	passwordSet.Flags().BoolVar(&setStdin, "password-stdin", false, "read the password from the first line of stdin")
	password.AddCommand(passwordSet)

	session := &cobra.Command{Use: "session", Short: "Inspect and revoke a user's sessions"}
	sessionList := &cobra.Command{
		Use:   "list <username>",
		Short: "List a user's sessions",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, a []string) error {
			return f.run(c, func(svc in.AdminService, ns string, mode outputMode) error {
				ss, err := svc.ListSessions(c.Context(), ns, a[0])
				if err != nil {
					return err
				}
				rows := make([]row, len(ss))
				for i, s := range ss {
					rows[i] = sessionRow(s)
				}
				return printRows(c.OutOrStdout(), mode, rows, true)
			})
		},
	}
	sessionRevoke := &cobra.Command{
		Use:   "revoke <session-id>",
		Short: "Revoke (delete) a session of this namespace",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, a []string) error {
			return f.run(c, func(svc in.AdminService, ns string, _ outputMode) error {
				return svc.RevokeSession(c.Context(), ns, a[0])
			})
		},
	}
	session.AddCommand(sessionList, sessionRevoke)

	cmd.AddCommand(create, list, show, disable, enable, password, session, syncCursor)
	return cmd
}
