package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/teamlead-com/cerbix/internal/buildinfo"
)

type versionExecutor func(io.Writer) error

func newVersionCommand(execute versionExecutor) *cobra.Command {
	cmd := &cobra.Command{
		Use:                   "version",
		Short:                 "Print build information.",
		Long:                  "Print version, commit, and Go toolchain information as indented JSON, then exit.",
		Example:               "cerbix version",
		GroupID:               rootGroupOther,
		Annotations:           map[string]string{allowExplicitFalseHelpAnnotation: "true"},
		Args:                  noArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVersionExecutor(cmd, execute)
		},
	}
	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if positional := firstVersionPositional(rawCLIArgs(cmd)); positional != "" {
			return usageExit(fmt.Errorf("version: unexpected argument %q", positional))
		}
		message := err.Error()
		if !strings.HasPrefix(message, "unknown flag:") && !strings.HasPrefix(message, "unknown shorthand flag:") {
			return err
		}
		if ctx := cmd.Context(); ctx != nil {
			boundary, _ := ctx.Value(parserBoundaryContextKey{}).(parserBoundary)
			if boundary.helpIntent.sawTrue || boundary.helpIntent.sawInvalid {
				return err
			}
		}
		return runVersionExecutor(cmd, execute)
	})
	return cmd
}

func runVersionExecutor(cmd *cobra.Command, execute versionExecutor) error {
	if err := execute(cmd.OutOrStdout()); err != nil {
		return runtimeExit(fmt.Errorf("version: %w", err))
	}
	return nil
}

func firstVersionPositional(args []string) string {
	if len(args) > 0 && args[0] == "version" {
		args = args[1:]
	}
	afterDash := false
	for _, arg := range args {
		if afterDash {
			return arg
		}
		if arg == "--" {
			afterDash = true
			continue
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return ""
}

func executeVersion(stdout io.Writer) error {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(buildinfo.Current())
}
