package main

import (
	"strconv"

	"github.com/spf13/cobra"

	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
)

type roomJSON struct {
	ID          string  `json:"id"`
	NamespaceID string  `json:"namespace_id"`
	Name        string  `json:"name"`
	CreatorID   string  `json:"creator_id"`
	JoinRule    string  `json:"join_rule"`
	Alias       *string `json:"alias"`
	MemberCount int     `json:"member_count"`
	CreatedAt   string  `json:"created_at"`
}

func roomRow(r *domain.Room) row {
	j := roomJSON{ID: r.ID, NamespaceID: r.NamespaceID, Name: r.Name, CreatorID: r.CreatorID,
		JoinRule: string(r.JoinRule), MemberCount: r.MemberCount, CreatedAt: ts(r.CreatedAt)}
	alias := "-"
	if r.Alias != "" {
		j.Alias, alias = &r.Alias, r.Alias
	}
	return row{id: r.ID, json: j, fields: [][2]string{
		{"id", r.ID}, {"name", r.Name}, {"join rule", j.JoinRule}, {"alias", alias},
		{"members", strconv.Itoa(r.MemberCount)}, {"created", j.CreatedAt},
	}}
}

type memberJSON struct {
	UserID    string `json:"user_id"`
	Username  string `json:"username"`
	State     string `json:"state"`
	Role      string `json:"role"`
	UpdatedAt string `json:"updated_at"`
}

func memberRow(username string, m *domain.Membership) row {
	j := memberJSON{UserID: m.UserID, Username: username, State: string(m.State), Role: string(m.Role), UpdatedAt: ts(m.UpdatedAt)}
	return row{id: m.UserID, json: j, fields: [][2]string{
		{"username", username}, {"user id", m.UserID}, {"state", j.State}, {"role", j.Role}, {"updated", j.UpdatedAt},
	}}
}

func newRoomCmd(f *adminFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "room",
		Short: "Manage rooms and memberships of a namespace",
		Long:  "Rooms are addressed by the ID that `room create` prints (room names are not unique).",
	}
	addNamespaceFlag(cmd, f)

	var owner, joinRule, alias string
	create := &cobra.Command{
		Use:     "create <name>",
		Short:   "Create a room owned by an existing user of the namespace",
		Example: "  nimlet room create general -n acme --owner alice",
		Args:    args(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, a []string) error {
			return f.run(c, func(svc in.AdminService, ns string, mode outputMode) error {
				r, err := svc.CreateRoom(c.Context(), in.AdminCreateRoomCmd{
					Namespace: ns, Name: a[0], Owner: owner, JoinRule: domain.JoinRule(joinRule), Alias: alias,
				})
				if err != nil {
					return err
				}
				return printRows(c.OutOrStdout(), mode, []row{roomRow(r)}, false)
			})
		},
	}
	create.Flags().StringVar(&owner, "owner", "", "username of the room owner (required)")
	create.Flags().StringVar(&joinRule, "join-rule", string(domain.JoinRuleInviteOnly), "invite_only or public")
	create.Flags().StringVar(&alias, "alias", "", "optional stable alias, unique in the namespace")

	var limit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List rooms (direct-message rooms excluded), newest first",
		Args:  args(cobra.NoArgs),
		RunE: func(c *cobra.Command, _ []string) error {
			return f.run(c, func(svc in.AdminService, ns string, mode outputMode) error {
				rs, err := svc.ListRooms(c.Context(), ns, limit)
				if err != nil {
					return err
				}
				rows := make([]row, len(rs))
				for i, r := range rs {
					rows[i] = roomRow(r)
				}
				return printRows(c.OutOrStdout(), mode, rows, true)
			})
		},
	}
	list.Flags().IntVar(&limit, "limit", 100, "maximum rooms to list")

	show := &cobra.Command{
		Use:   "show <room-id>",
		Short: "Show a room",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, a []string) error {
			return f.run(c, func(svc in.AdminService, ns string, mode outputMode) error {
				r, err := svc.GetRoom(c.Context(), ns, a[0])
				if err != nil {
					return err
				}
				return printRows(c.OutOrStdout(), mode, []row{roomRow(r)}, false)
			})
		},
	}

	member := &cobra.Command{Use: "member", Short: "Manage a room's members"}

	var role string
	var ifNotExists bool
	memberAdd := &cobra.Command{
		Use:   "add <room-id> <username>",
		Short: "Join a user to the room directly (not an invite)",
		Args:  args(cobra.ExactArgs(2)),
		RunE: func(c *cobra.Command, a []string) error {
			return f.run(c, func(svc in.AdminService, ns string, mode outputMode) error {
				m, err := svc.AddMember(c.Context(), ns, a[0], a[1], domain.MemberRole(role), ifNotExists)
				if err != nil {
					return err
				}
				return printRows(c.OutOrStdout(), mode, []row{memberRow(a[1], m)}, false)
			})
		},
	}
	memberAdd.Flags().StringVar(&role, "role", string(domain.RoleMember), "owner, admin, member, guest, agent or bot")
	memberAdd.Flags().BoolVar(&ifNotExists, "if-not-exists", false, "succeed (no change) if already a member")

	memberList := &cobra.Command{
		Use:   "list <room-id>",
		Short: "List the room's members (any state)",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, a []string) error {
			return f.run(c, func(svc in.AdminService, ns string, mode outputMode) error {
				ms, err := svc.ListMembers(c.Context(), ns, a[0])
				if err != nil {
					return err
				}
				rows := make([]row, len(ms))
				for i, m := range ms {
					rows[i] = memberRow(m.Username, &m.Membership)
				}
				return printRows(c.OutOrStdout(), mode, rows, true)
			})
		},
	}

	memberRemove := &cobra.Command{
		Use:   "remove <room-id> <username>",
		Short: "Remove a member: they leave and lose access to the room's history",
		Args:  args(cobra.ExactArgs(2)),
		RunE: func(c *cobra.Command, a []string) error {
			return f.run(c, func(svc in.AdminService, ns string, _ outputMode) error {
				return svc.RemoveMember(c.Context(), ns, a[0], a[1])
			})
		},
	}

	memberRole := &cobra.Command{Use: "role", Short: "Manage a member's role"}
	memberRoleSet := &cobra.Command{
		Use:     "set <room-id> <username> <role>",
		Short:   "Change a joined member's role",
		Example: "  nimlet room member role set <room-id> bob admin -n acme",
		Args:    args(cobra.ExactArgs(3)),
		RunE: func(c *cobra.Command, a []string) error {
			return f.run(c, func(svc in.AdminService, ns string, mode outputMode) error {
				m, err := svc.SetMemberRole(c.Context(), ns, a[0], a[1], domain.MemberRole(a[2]))
				if err != nil {
					return err
				}
				return printRows(c.OutOrStdout(), mode, []row{memberRow(a[1], m)}, false)
			})
		},
	}
	memberRole.AddCommand(memberRoleSet)
	member.AddCommand(memberAdd, memberList, memberRemove, memberRole)

	cmd.AddCommand(create, list, show, member)
	return cmd
}
