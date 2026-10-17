// Command nimlet is the single Nimlet binary: `nimlet server` runs the HTTP server,
// `nimlet migrate` manages the schema, and `nimlet namespace|user|room` administer data
// directly through the database (there is no HTTP admin API).
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/thiagoribeiro/nimlet/internal/app"
	appcfg "github.com/thiagoribeiro/nimlet/internal/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	err := newRootCmd().ExecuteContext(ctx)
	stop()
	os.Exit(report(os.Stderr, err))
}

// report prints err (if any) and returns the process exit code.
func report(w io.Writer, err error) int {
	if err == nil {
		return exitOK
	}
	// Cobra reports unknown commands as plain errors.
	if strings.HasPrefix(err.Error(), "unknown command") {
		err = usageError{err}
	}
	fmt.Fprintln(w, "error:", err)
	return exitCode(err)
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "nimlet",
		Short:         "Nimlet — lightweight, self-hosted messaging infrastructure",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })

	af := &adminFlags{}
	admin := []*cobra.Command{newNamespaceCmd(af), newUserCmd(af), newRoomCmd(af)}
	for _, c := range admin {
		c.PersistentFlags().StringVarP(&af.output, "output", "o", "human", "output format: human or json")
		c.PersistentFlags().BoolVarP(&af.quiet, "quiet", "q", false, "print only IDs")
		c.GroupID = "admin"
	}
	root.AddGroup(&cobra.Group{ID: "admin", Title: "Administration (needs NIMLET_ADMIN_DB_DSN):"})
	root.AddCommand(newServerCmd(), newMigrateCmd())
	root.AddCommand(admin...)
	return root
}

func newServerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "Run the HTTP server (applies pending migrations on startup)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
				Level: slog.LevelInfo,
			})))
			cfg, err := appcfg.Load()
			if err != nil {
				return err
			}
			return app.RunServer(cmd.Context(), cfg)
		},
	}
}

func newMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "migrate <up|down|status|version|reset|up-to|down-to> [version]",
		Short:     "Manage the database schema (needs only NIMLET_DB_DSN)",
		Args:      cobra.RangeArgs(1, 2),
		ValidArgs: []string{"up", "down", "status", "version", "reset", "up-to", "down-to"},
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := appcfg.LoadDB()
			if err != nil {
				return err
			}
			return app.Migrate(cmd.Context(), db.DSN, args[0], args[1:]...)
		},
	}
}
