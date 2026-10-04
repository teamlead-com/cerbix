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
		var boundary parserBoundary
		if ctx := cmd.Context(); ctx != nil {
			boundary, _ = ctx.Value(parserBoundaryContextKey{}).(parserBoundary)
			if boundary.target == cmd {
				if positional, found := firstVersionPositional(boundary.targetArgs); found {
					return usageExit(fmt.Errorf("version: unexpected argument %q", positional))
				}
			}
		}
		message := err.Error()
		if !strings.HasPrefix(message, "unknown flag:") && !strings.HasPrefix(message, "unknown shorthand flag:") {
			return err
		}
		if boundary.helpIntent.sawTrue || boundary.helpIntent.sawInvalid {
			return err
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

func firstVersionPositional(args []string) (value string, found bool) {
	afterDash := false
	for _, arg := range args {
		if afterDash {
			return arg, true
		}
		if arg == "--" {
			afterDash = true
			continue
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			return arg, true
		}
	}
	return "", false
}

func executeVersion(stdout io.Writer) error {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(buildinfo.Current())
}
