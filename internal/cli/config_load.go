package cli

import (
	"io"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/logging"
)

func loadConfigTo(path string, stderr io.Writer) *config.Config {
	cfg, err := config.Load(path)
	if err != nil {
		logging.Critical(logging.New(config.LogConfig{Level: "info", Format: "json"}, stderr),
			"config_load_failed", "path", path, "error", err.Error())
		return nil
	}
	return cfg
}
