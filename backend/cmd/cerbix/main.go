package main

import (
	"os"

	"git.example.com/monitoring/cerbix/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
