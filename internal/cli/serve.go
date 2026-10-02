package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

type serveOptions struct {
	ConfigPath string
	Role       string
	Region     string
}

type serveExecutor func(serveOptions, io.Writer, io.Writer) error

func executeServe(opts serveOptions, stdout, stderr io.Writer) error {
	code := serveRuntime(opts, stdout, stderr)
	if code == 0 {
		return nil
	}
	return printedExit(code)
}

func newServeCommand(execute serveExecutor) *cobra.Command {
	opts := serveOptions{Role: "all"}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run cerbix services.",
		Long: "Run cerbix in one process role. Role all runs the local in-process stack; api, scheduler, and " +
			"worker are distributed roles; agent is the broker-less HTTP-pull prober. Region selects the worker " +
			"or agent pool and an empty value means core.",
		Example: "cerbix serve --config /etc/cerbix/config.yaml --role all\n" +
			"cerbix serve --config /etc/cerbix/worker.yaml --role worker --region eu-west",
		GroupID:               rootGroupRuntime,
		Args:                  noArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateRequiredFlags(cmd); err != nil {
				return err
			}
			if !validRoles[opts.Role] {
				return usageExit(fmt.Errorf("serve: invalid --role %q", opts.Role))
			}
			return execute(opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().SortFlags = false
	addStringFlag(cmd, &opts.ConfigPath, "config", "", "path", "path to config YAML", true)
	addStringFlag(cmd, &opts.Role, "role", "all", "all|api|scheduler|worker|agent", "process role: all|api|scheduler|worker|agent", false)
	addStringFlag(cmd, &opts.Region, "region", "", "name", "worker or agent region; empty = core", false)
	setHelpSection(cmd, helpAnnotationExitCodes, "0  Services stopped cleanly.\n1  Configuration, startup, dependency, runtime, or shutdown failure.\n2  CLI usage error.")
	return cmd
}
