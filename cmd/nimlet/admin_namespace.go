package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
)

type namespaceJSON struct {
	ID                string  `json:"id"`
	Slug              string  `json:"slug"`
	Status            string  `json:"status"`
	AllowRegistration bool    `json:"allow_registration"`
	PushMode          string  `json:"push_mode"`
	CreatedAt         string  `json:"created_at"`
	DisabledAt        *string `json:"disabled_at"`
}

func namespaceRow(n *domain.Namespace) row {
	j := namespaceJSON{ID: n.ID, Slug: n.Slug, Status: string(n.Status), AllowRegistration: n.AllowRegistration, PushMode: string(n.PushMode), CreatedAt: ts(n.CreatedAt)}
	disabled := "-"
	if n.DisabledAt != nil {
		d := ts(*n.DisabledAt)
		j.DisabledAt, disabled = &d, d
	}
	return row{id: n.ID, json: j, fields: [][2]string{
		{"slug", n.Slug}, {"id", n.ID}, {"status", string(n.Status)},
		{"registration", strconv.FormatBool(n.AllowRegistration)}, {"push", string(n.PushMode)}, {"created", j.CreatedAt}, {"disabled", disabled},
	}}
}

func newNamespaceCmd(f *adminFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "namespace",
		Short: "Manage namespaces (isolated tenants)",
	}

	printOne := func(c *cobra.Command, n *domain.Namespace) error {
		mode, err := f.mode()
		if err != nil {
			return err
		}
		return printRows(c.OutOrStdout(), mode, []row{namespaceRow(n)}, false)
	}

	var allowRegistration bool
	create := &cobra.Command{
		Use:     "create <slug>",
		Short:   "Create a namespace",
		Example: "  nimlet namespace create acme",
		Args:    args(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, a []string) error {
			return withAdmin(c, func(svc in.AdminService) error {
				n, err := svc.CreateNamespace(c.Context(), a[0], allowRegistration)
				if err != nil {
					return err
				}
				return printOne(c, n)
			})
		},
	}
	create.Flags().BoolVar(&allowRegistration, "allow-registration", false, "accept public POST /register in this namespace")

	list := &cobra.Command{
		Use:   "list",
		Short: "List namespaces",
		Args:  args(cobra.NoArgs),
		RunE: func(c *cobra.Command, _ []string) error {
			mode, err := f.mode()
			if err != nil {
				return err
			}
			return withAdmin(c, func(svc in.AdminService) error {
				nss, err := svc.ListNamespaces(c.Context())
				if err != nil {
					return err
				}
				rows := make([]row, len(nss))
				for i, n := range nss {
					rows[i] = namespaceRow(n)
				}
				return printRows(c.OutOrStdout(), mode, rows, true)
			})
		},
	}

	show := &cobra.Command{
		Use:   "show <slug>",
		Short: "Show a namespace",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, a []string) error {
			return withAdmin(c, func(svc in.AdminService) error {
				n, err := svc.GetNamespace(c.Context(), a[0])
				if err != nil {
					return err
				}
				return printOne(c, n)
			})
		},
	}

	var confirm string
	disable := &cobra.Command{
		Use:   "disable <slug>",
		Short: "Disable a namespace: no logins, requests, sync or AppServices until re-enabled",
		Long: "Disable a namespace. Every session of it stops working on its next request; data is kept.\n" +
			"Requires --confirm <slug> to guard against disabling the wrong namespace.",
		Example: "  nimlet namespace disable acme --confirm acme",
		Args:    args(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, a []string) error {
			if confirm != a[0] {
				return usageError{fmt.Errorf("refusing to disable %q: pass --confirm %s", a[0], a[0])}
			}
			return withAdmin(c, func(svc in.AdminService) error {
				n, err := svc.SetNamespaceStatus(c.Context(), a[0], domain.NamespaceDisabled)
				if err != nil {
					return err
				}
				return printOne(c, n)
			})
		},
	}
	disable.Flags().StringVar(&confirm, "confirm", "", "the namespace slug again, to confirm")

	enable := &cobra.Command{
		Use:   "enable <slug>",
		Short: "Re-enable a disabled namespace",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, a []string) error {
			return withAdmin(c, func(svc in.AdminService) error {
				n, err := svc.SetNamespaceStatus(c.Context(), a[0], domain.NamespaceActive)
				if err != nil {
					return err
				}
				return printOne(c, n)
			})
		},
	}

	registration := &cobra.Command{
		Use:     "registration <slug> <on|off>",
		Short:   "Turn public POST /register on or off for a namespace",
		Example: "  nimlet namespace registration nimchat on",
		Args:    args(cobra.ExactArgs(2)),
		RunE: func(c *cobra.Command, a []string) error {
			var allow bool
			switch a[1] {
			case "on":
				allow = true
			case "off":
			default:
				return usageError{fmt.Errorf("expected on or off, got %q", a[1])}
			}
			return withAdmin(c, func(svc in.AdminService) error {
				n, err := svc.SetNamespaceRegistration(c.Context(), a[0], allow)
				if err != nil {
					return err
				}
				return printOne(c, n)
			})
		},
	}

	push := &cobra.Command{
		Use:   "push <slug> <digest|message>",
		Short: "Choose how offline members are notified",
		Long: "digest: one coalesced push with room names and counts, never message content.\n" +
			"message: one push per message with a short preview; the preview transits FCM.",
		Example: "  nimlet namespace push default message",
		Args:    args(cobra.ExactArgs(2)),
		RunE: func(c *cobra.Command, a []string) error {
			mode, err := domain.ParsePushMode(a[1])
			if err != nil {
				return usageError{err}
			}
			return withAdmin(c, func(svc in.AdminService) error {
				n, err := svc.SetNamespacePushMode(c.Context(), a[0], mode)
				if err != nil {
					return err
				}
				return printOne(c, n)
			})
		},
	}

	cmd.AddCommand(create, list, show, disable, enable, registration, push)
	return cmd
}
