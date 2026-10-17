package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/thiagoribeiro/nimlet/internal/app"
	appcfg "github.com/thiagoribeiro/nimlet/internal/config"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
)

// Exit codes are part of the CLI contract: scripts branch on them.
const (
	exitOK          = 0
	exitFailure     = 1
	exitUsage       = 2 // invalid argument or flag
	exitNotFound    = 3
	exitConflict    = 4
	exitForbidden   = 5 // not allowed / invalid credentials
	exitUnavailable = 6 // a dependency (the database) could not be reached
)

// usageError marks command-line misuse (bad flags/args), reported with exitUsage.
type usageError struct{ error }

func exitCode(err error) int {
	var ue usageError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &ue), errors.Is(err, domain.ErrInvalidArgument):
		return exitUsage
	case errors.Is(err, domain.ErrNotFound):
		return exitNotFound
	case errors.Is(err, domain.ErrAlreadyExists):
		return exitConflict
	case errors.Is(err, domain.ErrPermissionDenied), errors.Is(err, domain.ErrInvalidPassword):
		return exitForbidden
	case errors.Is(err, app.ErrDependency):
		return exitUnavailable
	default:
		return exitFailure
	}
}

// args wraps a cobra positional-args validator so violations exit with exitUsage.
func args(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, a []string) error {
		if err := v(cmd, a); err != nil {
			return usageError{err}
		}
		return nil
	}
}

// --- output ---

type outputMode int

const (
	outputHuman outputMode = iota
	outputJSON
	outputQuiet // IDs only, one per line
)

type adminFlags struct {
	output    string
	quiet     bool
	namespace string
}

func (f *adminFlags) mode() (outputMode, error) {
	if f.quiet {
		return outputQuiet, nil
	}
	switch f.output {
	case "", "human":
		return outputHuman, nil
	case "json":
		return outputJSON, nil
	}
	return 0, usageError{fmt.Errorf("--output must be human or json, got %q", f.output)}
}

// ns returns the target namespace: --namespace, else NIMLET_NAMESPACE. There is no implicit
// default — an operator must always say which namespace they are changing.
func (f *adminFlags) ns() (string, error) {
	if f.namespace != "" {
		return f.namespace, nil
	}
	if env := os.Getenv("NIMLET_NAMESPACE"); env != "" {
		return env, nil
	}
	return "", usageError{errors.New("namespace required: pass -n/--namespace or set NIMLET_NAMESPACE")}
}

// addNamespaceFlag adds -n/--namespace to a command group.
func addNamespaceFlag(cmd *cobra.Command, f *adminFlags) {
	cmd.PersistentFlags().StringVarP(&f.namespace, "namespace", "n", "", "target namespace slug (or NIMLET_NAMESPACE)")
}

// row is one record of output: an ID for --quiet, a value for --output json, and ordered
// key/value pairs for humans.
type row struct {
	id     string
	json   any
	fields [][2]string
}

func printRows(w io.Writer, mode outputMode, rows []row, list bool) error {
	switch mode {
	case outputQuiet:
		for _, r := range rows {
			fmt.Fprintln(w, r.id)
		}
		return nil
	case outputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if list {
			out := make([]any, len(rows))
			for i, r := range rows {
				out[i] = r.json
			}
			return enc.Encode(out)
		}
		if len(rows) == 0 {
			return enc.Encode(nil)
		}
		return enc.Encode(rows[0].json)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if list {
		if len(rows) == 0 {
			fmt.Fprintln(w, "(none)")
			return nil
		}
		for i, kv := range rows[0].fields {
			if i > 0 {
				fmt.Fprint(tw, "\t")
			}
			fmt.Fprint(tw, strings.ToUpper(kv[0]))
		}
		fmt.Fprintln(tw)
		for _, r := range rows {
			for i, kv := range r.fields {
				if i > 0 {
					fmt.Fprint(tw, "\t")
				}
				fmt.Fprint(tw, kv[1])
			}
			fmt.Fprintln(tw)
		}
	} else {
		for _, r := range rows {
			for _, kv := range r.fields {
				fmt.Fprintf(tw, "%s:\t%s\n", kv[0], kv[1])
			}
		}
	}
	return tw.Flush()
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// --- service plumbing ---

// run resolves the target namespace and output mode, then calls fn with the admin service:
// the common shape of every namespaced command.
func (f *adminFlags) run(c *cobra.Command, fn func(svc in.AdminService, ns string, mode outputMode) error) error {
	ns, err := f.ns()
	if err != nil {
		return err
	}
	mode, err := f.mode()
	if err != nil {
		return err
	}
	return withAdmin(c, func(svc in.AdminService) error { return fn(svc, ns, mode) })
}

// withAdmin opens the admin service for one command invocation.
func withAdmin(cmd *cobra.Command, fn func(svc in.AdminService) error) error {
	dsn, err := appcfg.LoadAdminDSN()
	if err != nil {
		return usageError{err}
	}
	svc, closeFn, err := app.OpenAdmin(cmd.Context(), dsn, uuid.NewString())
	if err != nil {
		return err
	}
	defer closeFn()
	return fn(svc)
}

// readPassword takes a password from stdin (--password-stdin: first line) or, on a
// terminal, from an echo-less prompt entered twice. Passwords are never accepted as
// arguments: they would end up in shell history and process listings.
func readPassword(cmd *cobra.Command, fromStdin bool) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		pw := strings.TrimRight(line, "\r\n")
		if pw == "" {
			return "", usageError{errors.New("empty password on stdin")}
		}
		return pw, nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", usageError{errors.New("no terminal for a password prompt: use --password-stdin")}
	}
	errOut := cmd.ErrOrStderr()
	fmt.Fprint(errOut, "Password: ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(errOut)
	if err != nil {
		return "", err
	}
	fmt.Fprint(errOut, "Repeat password: ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(errOut)
	if err != nil {
		return "", err
	}
	if string(first) != string(second) {
		return "", usageError{errors.New("passwords do not match")}
	}
	if len(first) == 0 {
		return "", usageError{errors.New("empty password")}
	}
	return string(first), nil
}
