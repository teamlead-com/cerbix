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

type enqueueServiceRepairOptions struct {
	ConfigPath string
	ProjectID  string
	ServiceID  string
	From       string
	To         string
	FromTime   time.Time
	ToTime     time.Time
}

type enqueueServiceRepairExecutor func(enqueueServiceRepairOptions, io.Writer, io.Writer) error

func newEnqueueServiceRepairCommand(execute enqueueServiceRepairExecutor) *cobra.Command {
	var opts enqueueServiceRepairOptions
	cmd := &cobra.Command{
		Use:   "enqueue-service-repair",
		Short: "Queue a bounded service repair range.",
		Long: "Queue a durable admin repair for one service and RFC3339 range. The store normalizes and " +
			"coalesces pending work; the scheduler leader recomputes it later through the normal audited repair " +
			"machinery. There is no dry-run, confirmation, or immediate recompute.",
		Example: "cerbix enqueue-service-repair --config /etc/cerbix/config.yaml " +
			"--project 00000000-0000-4000-8000-000000000001 --service 00000000-0000-4000-8000-000000000002 " +
			"--from 2026-01-01T00:00:00Z --to 2026-01-01T01:00:00Z",
		GroupID:               rootGroupMaintenance,
		Args:                  noArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateRequiredFlags(cmd); err != nil {
				return err
			}
			from, err := time.Parse(time.RFC3339, opts.From)
			if err != nil {
				return usageExit(fmt.Errorf("enqueue-service-repair: --from must be RFC3339: %w", err))
			}
			to, err := time.Parse(time.RFC3339, opts.To)
			if err != nil {
				return usageExit(fmt.Errorf("enqueue-service-repair: --to must be RFC3339: %w", err))
			}
			if !to.After(from) {
				return usageExit(fmt.Errorf("enqueue-service-repair: --to must be after --from"))
			}
			opts.FromTime = from
			opts.ToTime = to
			return execute(opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().SortFlags = false
	addStringFlag(cmd, &opts.ConfigPath, "config", "", "path", "path to config YAML", true)
	addStringFlag(cmd, &opts.ProjectID, "project", "", "id", "project id", true)
	addStringFlag(cmd, &opts.ServiceID, "service", "", "id", "service id", true)
	addStringFlag(cmd, &opts.From, "from", "", "RFC3339", "range start (floored to the bucket)", true)
	addStringFlag(cmd, &opts.To, "to", "", "RFC3339", "range end (ceiled to the bucket)", true)
	setHelpSection(cmd, helpAnnotationExitCodes, "0  Repair range durably queued.\n1  Configuration, database, or enqueue failure.\n2  CLI usage error.")
	return cmd
}

func executeEnqueueServiceRepair(opts enqueueServiceRepairOptions, stdout, stderr io.Writer) error {
	cfg := loadConfigTo(opts.ConfigPath, stderr)
	if cfg == nil {
		return printedExit(1)
	}
	logger := logging.New(cfg.Log, stdout)
	if cfg.Database.DSN == "" {
		logging.Critical(logger, "enqueue_service_repair_requires_database", "hint", "set database.dsn")
		return printedExit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := store.Open(ctx, cfg.Database.DSN)
	if err != nil {
		logging.Critical(logger, "db_connect_failed", "error", err.Error())
		return printedExit(1)
	}
	defer st.Close()
	if err := st.EnqueueRepairRange(ctx, opts.ProjectID, opts.ServiceID, opts.FromTime, opts.ToTime, store.ReasonAdmin); err != nil {
		logging.Critical(logger, "enqueue_service_repair_failed", "error", err.Error())
		return printedExit(1)
	}
	logger.Info("service_repair_enqueued", "project", opts.ProjectID, "service", opts.ServiceID,
		"from", opts.FromTime.UTC().Format(time.RFC3339), "to", opts.ToTime.UTC().Format(time.RFC3339))
	return nil
}
