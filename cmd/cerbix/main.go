package main

import (
	"os"

	"github.com/teamlead-com/cerbix/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
