package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"github.com/teamlead-com/cerbix/internal/logging"
	"github.com/teamlead-com/cerbix/internal/store"
)

const adoptFactMonthContextMargin = 30 * time.Minute

type adoptFactMonthOptions struct {
	ConfigPath string
	Month      string
	MonthStart time.Time
	Timeout    time.Duration
}

type adoptFactMonthExecutor func(adoptFactMonthOptions, io.Writer, io.Writer) error

func newAdoptFactMonthCommand(execute adoptFactMonthExecutor) *cobra.Command {
	opts := adoptFactMonthOptions{Timeout: 10 * time.Minute}
	cmd := &cobra.Command{
		Use:   "adopt-fact-month",
		Short: "Adopt a retained fact partition.",
		Long: "Adopt one YYYY-MM month of service-reliability facts from the DEFAULT partition through the " +
			"copy-authoritative recovery path. There is no dry-run or confirmation; use a maintenance window " +
			"because the fenced cutover holds the parent lock through commit.",
		Example:               "cerbix adopt-fact-month --config /etc/cerbix/config.yaml --month 2026-01",
		GroupID:               rootGroupMaintenance,
		Args:                  noArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateRequiredFlags(cmd); err != nil {
				return err
			}
			month, err := time.ParseInLocation("2006-01", opts.Month, time.UTC)
			if err != nil {
				return usageExit(fmt.Errorf("adopt-fact-month: --month must be YYYY-MM: %w", err))
			}
			if opts.Timeout <= 0 {
				return usageExit(fmt.Errorf("adopt-fact-month: --timeout must be positive"))
			}
			if opts.Timeout > time.Duration(1<<63-1)-adoptFactMonthContextMargin {
				return usageExit(fmt.Errorf("adopt-fact-month: --timeout is too large"))
			}
			opts.MonthStart = month
			return execute(opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().SortFlags = false
	addStringFlag(cmd, &opts.ConfigPath, "config", "", "path", "path to config YAML", true)
	addStringFlag(cmd, &opts.Month, "month", "", "YYYY-MM", "month to adopt", true)
	addDurationFlag(cmd, &opts.Timeout, "timeout", 10*time.Minute, "total budget for the fenced cutover (parent lock through commit)")
	setHelpSection(cmd, helpAnnotationExitCodes, "0  Month adopted or already attached.\n1  Configuration, database, or adoption failure.\n2  CLI usage error.")
	return cmd
}

func executeAdoptFactMonth(opts adoptFactMonthOptions, stdout, stderr io.Writer) error {
	cfg := loadConfigTo(opts.ConfigPath, stderr)
	if cfg == nil {
		return printedExit(1)
	}
	logger := logging.New(cfg.Log, stdout)
	if cfg.Database.DSN == "" {
		logging.Critical(logger, "adopt_fact_month_requires_database", "hint", "set database.dsn")
		return printedExit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout+adoptFactMonthContextMargin)
	defer cancel()
	st, err := store.Open(ctx, cfg.Database.DSN)
	if err != nil {
		logging.Critical(logger, "db_connect_failed", "error", err.Error())
		return printedExit(1)
	}
	defer st.Close()
	if err := st.AdoptServiceFactMonthOperator(ctx, opts.MonthStart, opts.Timeout); err != nil {
		logging.Critical(logger, "adopt_fact_month_failed", "month", opts.Month, "error", err.Error())
		return printedExit(1)
	}
	logger.Info("adopt_fact_month_complete", "month", opts.Month)
	return nil
}
