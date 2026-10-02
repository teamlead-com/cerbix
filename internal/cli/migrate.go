package cli

import (
	"context"
	"io"
	"time"

	"github.com/spf13/cobra"
	"github.com/teamlead-com/cerbix/internal/logging"
	"github.com/teamlead-com/cerbix/internal/store"
)

type migrateOptions struct {
	ConfigPath string
}

type migrateExecutor func(migrateOptions, io.Writer, io.Writer) error

func newMigrateCommand(execute migrateExecutor) *cobra.Command {
	var opts migrateOptions
	cmd := &cobra.Command{
		Use:                   "migrate",
		Short:                 "Apply database migrations and exit.",
		Long:                  "Apply the embedded database migrations and exit without starting runtime services.",
		Example:               "cerbix migrate --config /etc/cerbix/config.yaml",
		GroupID:               rootGroupRuntime,
		Args:                  noArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateRequiredFlags(cmd); err != nil {
				return err
			}
			return execute(opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().SortFlags = false
	addStringFlag(cmd, &opts.ConfigPath, "config", "", "path", "path to config YAML", true)
	setHelpSection(cmd, helpAnnotationExitCodes, "0  Migrations applied.\n1  Configuration, database, or migration failure.\n2  CLI usage error.")
	return cmd
}

func executeMigrate(opts migrateOptions, stdout, stderr io.Writer) error {
	cfg := loadConfigTo(opts.ConfigPath, stderr)
	if cfg == nil {
		return printedExit(1)
	}
	logger := logging.New(cfg.Log, stdout)
	if cfg.Database.DSN == "" {
		logging.Critical(logger, "migrate_requires_database", "hint", "set database.dsn")
		return printedExit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, cfg.Database.DSN); err != nil {
		logging.Critical(logger, "db_migrate_failed", "error", err.Error())
		return printedExit(1)
	}
	logger.Info("migrations_applied")
	return nil
}
