package cli

import (
	"context"
	"io"
	"time"

	"github.com/spf13/cobra"
	"github.com/teamlead-com/cerbix/internal/logging"
	"github.com/teamlead-com/cerbix/internal/secret"
	"github.com/teamlead-com/cerbix/internal/store"
)

// reencryptExitRowsSkipped: every table was processed, but some phone numbers could not be rehashed
// now and stay under the previous key (FR-041a pre-review 2): keep that key and run again.
const reencryptExitRowsSkipped = 3

type reencryptOptions struct {
	ConfigPath string
}

type reencryptExecutor func(reencryptOptions, io.Writer, io.Writer) error

func newReencryptCommand(execute reencryptExecutor) *cobra.Command {
	var opts reencryptOptions
	cmd := &cobra.Command{
		Use:                   "reencrypt",
		Short:                 "Re-encrypt stored secrets.",
		Long:                  "Re-encrypt stored secrets with the current primary at-rest key. Keep the previous key configured until this command succeeds.",
		Example:               "cerbix reencrypt --config /etc/cerbix/config.yaml",
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
	setHelpSection(cmd, helpAnnotationExitCodes, "0  Stored secrets re-encrypted.\n1  Configuration, database, key, or rewrite failure.\n2  CLI usage error.\n"+
		"3  Completed, but some phone numbers were skipped: keep the previous key and run again.")
	return cmd
}

func executeReencrypt(opts reencryptOptions, stdout, stderr io.Writer) error {
	cfg := loadConfigTo(opts.ConfigPath, stderr)
	if cfg == nil {
		return printedExit(1)
	}
	logger := logging.New(cfg.Log, stdout)
	if cfg.Database.DSN == "" {
		logging.Critical(logger, "reencrypt_requires_database", "hint", "set database.dsn")
		return printedExit(1)
	}
	keys, err := cfg.Security.Keys()
	if err != nil {
		logging.Critical(logger, "encryption_key_invalid", "error", err.Error())
		return printedExit(1)
	}
	if keys == nil {
		logging.Critical(logger, "reencrypt_requires_key", "hint", "set security.encryption_key")
		return printedExit(1)
	}
	cipher, err := secret.New(keys...)
	if err != nil {
		logging.Critical(logger, "cipher_init_failed", "error", err.Error())
		return printedExit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	st, err := store.Open(ctx, cfg.Database.DSN)
	if err != nil {
		logging.Critical(logger, "db_connect_failed", "error", err.Error())
		return printedExit(1)
	}
	defer st.Close()
	st.WithCipher(cipher).WithLogger(logger)
	res, err := st.ReencryptSecrets(ctx)
	if err != nil {
		logging.Critical(logger, "reencrypt_failed", "error", err.Error())
		return printedExit(1)
	}
	logger.Info("reencrypt_complete", "webhooks", res.Webhooks, "channels", res.Channels,
		"phone_numbers_released", res.PhoneNumbersReleased, "phone_rehashes_skipped", res.PhoneRehashesSkipped)
	if res.PhoneRehashesSkipped > 0 {
		// Every table was processed, but the skipped rows (each logged by user id) are still under the
		// previous key: dropping it now would leave those numbers undecryptable and stop every later
		// run at their decrypt. Exit 3 — not 0, not a failure — says "keep the key, run again"; the
		// skips are transient (a row in use), so the next run normally exits 0.
		logger.Warn("reencrypt_phone_rehashes_skipped", "count", res.PhoneRehashesSkipped,
			"hint", "keep the previous key configured and run cerbix reencrypt again")
		return printedExit(reencryptExitRowsSkipped)
	}
	return nil
}
