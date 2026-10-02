package cli

import "io"

func runGate(args []string, stdout, stderr io.Writer) int {
	cmd := newGateCommand(executeGateCheck)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return executeCommand(cmd, args, stderr)
}

func runChange(args []string, stdout, stderr io.Writer) int {
	cmd := newChangeCommand(executeChangeRecord)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return executeCommand(cmd, args, stderr)
}
