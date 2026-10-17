package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thiagoribeiro/nimlet/internal/app"
	"github.com/thiagoribeiro/nimlet/internal/domain"
)

func TestExitCode(t *testing.T) {
	wrap := func(err error) error { return fmt.Errorf("context: %w", err) }
	for err, want := range map[error]int{
		nil:                                 exitOK,
		usageError{errors.New("bad flag")}:  exitUsage,
		wrap(domain.ErrInvalidArgument):     exitUsage,
		wrap(domain.ErrNotFound):            exitNotFound,
		wrap(domain.ErrAlreadyExists):       exitConflict,
		wrap(domain.ErrPermissionDenied):    exitForbidden,
		wrap(app.ErrDependency):             exitUnavailable,
		errors.New("something else failed"): exitFailure,
	} {
		if got := exitCode(err); got != want {
			t.Errorf("exitCode(%v) = %d, want %d", err, got, want)
		}
	}
}

func TestCLI_UsageErrorsNeedNoDatabase(t *testing.T) {
	t.Setenv("NIMLET_ADMIN_DB_DSN", "")
	t.Setenv("NIMLET_NAMESPACE", "")
	for _, argv := range [][]string{
		{"bogus"},
		{"user", "list"},                            // no namespace
		{"user", "list", "-n", "x", "--nope"},       // unknown flag
		{"namespace", "create"},                     // missing slug
		{"namespace", "disable", "x"},               // no --confirm
		{"namespace", "registration", "x", "maybe"}, // bad value
		{"user", "list", "-n", "x"},                 // no NIMLET_ADMIN_DB_DSN
	} {
		var errOut bytes.Buffer
		cmd := newRootCmd()
		cmd.SetArgs(argv)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&errOut)
		if code := report(&errOut, cmd.Execute()); code != exitUsage {
			t.Errorf("nimlet %s: exit %d, want %d (%s)", strings.Join(argv, " "), code, exitUsage, errOut.String())
		}
	}
}
