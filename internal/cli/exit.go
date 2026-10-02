package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

type ExitError struct {
	Code    int
	Err     error
	Printed bool
}

func (e *ExitError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func usageExit(err error) error {
	return &ExitError{Code: 2, Err: err}
}

func runtimeExit(err error) error {
	return &ExitError{Code: 1, Err: err}
}

func printedExit(code int) error {
	return &ExitError{Code: code, Printed: true}
}

func exitFromCode(code int) error {
	if code == 0 {
		return nil
	}
	return printedExit(code)
}

type rawCLIArgsContextKey struct{}

func executeCommand(root *cobra.Command, args []string, stderr io.Writer) int {
	boundary, err := inspectParserBoundary(root, args)
	if err != nil {
		return commandErrorCode(err, stderr)
	}
	ctx := context.WithValue(context.Background(), rawCLIArgsContextKey{}, append([]string(nil), args...))
	ctx = context.WithValue(ctx, parserBoundaryContextKey{}, boundary)
	root.SetContext(ctx)
	boundary.target.SetContext(ctx)
	if handled, err := executeRequestedHelp(root, boundary); handled {
		return commandErrorCode(err, stderr)
	}
	root.SetArgs(args)
	return commandErrorCode(root.Execute(), stderr)
}

func commandErrorCode(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		if !exitErr.Printed && exitErr.Err != nil {
			_, _ = fmt.Fprintln(stderr, exitErr.Err)
		}
		return exitErr.Code
	}
	_, _ = fmt.Fprintln(stderr, err)
	return 2
}

func rawCLIArgs(cmd *cobra.Command) []string {
	if cmd.Context() == nil {
		return nil
	}
	args, _ := cmd.Context().Value(rawCLIArgsContextKey{}).([]string)
	return args
}

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return usageExit(fmt.Errorf("%s: unexpected argument %q", commandDisplayPath(cmd), args[0]))
}

func unknownSubcommandArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: unknown subcommand %q\n", commandDisplayPath(cmd), args[0])
	if err := writeCommandHelp(cmd, cmd.ErrOrStderr()); err != nil {
		return runtimeExit(fmt.Errorf("help: %w", err))
	}
	return printedExit(2)
}

func validateRequiredFlags(cmd *cobra.Command) error {
	missing := missingRequiredFlags(cmd)
	if len(missing) == 0 {
		return nil
	}
	verb := "is"
	if len(missing) > 1 {
		verb = "are"
	}
	return usageExit(fmt.Errorf("%s: %s %s required", commandDisplayPath(cmd), joinFlagNames(missing), verb))
}

func validateRequiredFlagsWithMessage(cmd *cobra.Command, message string) error {
	if len(missingRequiredFlags(cmd)) == 0 {
		return nil
	}
	return usageExit(errors.New(message))
}

func missingRequiredFlags(cmd *cobra.Command) []string {
	var missing []string
	for _, name := range commandFlagNames(cmd) {
		flag := cmd.Flags().Lookup(name)
		if flagIsRequired(cmd, name) && flag.Value.String() == "" {
			missing = append(missing, "--"+name)
		}
	}
	return missing
}

func commandDisplayPath(cmd *cobra.Command) string {
	path := cmd.CommandPath()
	if cmd.Root().Name() == "cerbix" {
		return strings.TrimPrefix(path, "cerbix ")
	}
	return path
}

func joinFlagNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}
