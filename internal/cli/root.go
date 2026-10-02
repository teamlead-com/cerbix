package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

const (
	rootGroupRuntime          = "runtime"
	rootGroupMaintenance      = "maintenance"
	rootGroupCICD             = "cicd"
	rootGroupOther            = "other"
	commandOrderAnnotationKey = "cerbix.io/command-order"
)

func newRootCommand(stdout, stderr io.Writer) *cobra.Command {
	return newRootCommandWithVersionExecutor(stdout, stderr, executeVersion)
}

func newRootCommandWithVersionExecutor(stdout, stderr io.Writer, versionExecute versionExecutor) *cobra.Command {
	root := &cobra.Command{
		Use:                "cerbix",
		Short:              "self-hosted service reliability platform",
		SilenceUsage:       true,
		SilenceErrors:      true,
		DisableSuggestions: true,
		Args:               noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := writeRootHelp(cmd, cmd.ErrOrStderr()); err != nil {
				return runtimeExit(fmt.Errorf("help: %w", err))
			}
			return printedExit(2)
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddGroup(
		&cobra.Group{ID: rootGroupRuntime, Title: "Runtime:"},
		&cobra.Group{ID: rootGroupMaintenance, Title: "Reliability maintenance:"},
		&cobra.Group{ID: rootGroupCICD, Title: "CI/CD:"},
		&cobra.Group{ID: rootGroupOther, Title: "Other:"},
	)

	serve := newServeCommand(executeServe)
	migrate := newMigrateCommand(executeMigrate)
	reencrypt := newReencryptCommand(executeReencrypt)
	adopt := newAdoptFactMonthCommand(executeAdoptFactMonth)
	repair := newEnqueueServiceRepairCommand(executeEnqueueServiceRepair)
	gate := newGateCommand(executeGateCheck)
	change := newChangeCommand(executeChangeRecord)
	version := newVersionCommand(versionExecute)
	addOrderedCommands(root, serve, migrate, reencrypt, adopt, repair, gate, change, version)
	root.SetHelpCommand(newHelpCommand(root))

	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		if cmd == root {
			_ = writeRootHelp(cmd, cmd.OutOrStdout())
			return
		}
		_ = writeCommandHelp(cmd, cmd.OutOrStdout())
	})
	return root
}

func addOrderedCommands(parent *cobra.Command, children ...*cobra.Command) {
	parent.AddCommand(children...)
	if parent.Annotations == nil {
		parent.Annotations = make(map[string]string)
	}
	order := commandOrderNames(parent)
	for _, child := range children {
		order = append(order, child.Name())
	}
	parent.Annotations[commandOrderAnnotationKey] = strings.Join(order, ",")
}

func commandOrderNames(parent *cobra.Command) []string {
	if parent.Annotations == nil || parent.Annotations[commandOrderAnnotationKey] == "" {
		return nil
	}
	return strings.Split(parent.Annotations[commandOrderAnnotationKey], ",")
}

func orderedChildCommands(parent *cobra.Command) []*cobra.Command {
	byName := make(map[string]*cobra.Command)
	for _, child := range parent.Commands() {
		byName[child.Name()] = child
	}
	ordered := make([]*cobra.Command, 0, len(byName))
	for _, name := range commandOrderNames(parent) {
		if child := byName[name]; child != nil {
			ordered = append(ordered, child)
		}
	}
	return ordered
}

func orderedVisibleChildCommands(parent *cobra.Command) []*cobra.Command {
	var visible []*cobra.Command
	for _, child := range orderedChildCommands(parent) {
		if child.Name() == "help" || !child.IsAvailableCommand() {
			continue
		}
		visible = append(visible, child)
	}
	return visible
}

type rootHelpEntry struct {
	path  string
	short string
}

func writeRootHelp(root *cobra.Command, w io.Writer) error {
	if _, err := fmt.Fprintln(w, "cerbix — self-hosted service reliability platform"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "\nUsage:\n  cerbix <command> [flags]"); err != nil {
		return err
	}
	for _, group := range root.Groups() {
		entries := rootHelpEntries(root, group.ID)
		if len(entries) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "\n%s\n", group.Title); err != nil {
			return err
		}
		for _, entry := range entries {
			if _, err := fmt.Fprintf(w, "  %-23s %s\n", entry.path, entry.short); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintln(w, "\nRun `cerbix <command> --help` for command-specific flags and examples.")
	return err
}

func rootHelpEntries(root *cobra.Command, groupID string) []rootHelpEntry {
	var entries []rootHelpEntry
	var walk func(*cobra.Command, string)
	walk = func(parent *cobra.Command, inheritedGroup string) {
		for _, child := range orderedVisibleChildCommands(parent) {
			childGroup := inheritedGroup
			if parent == root && child.GroupID != "" {
				childGroup = child.GroupID
			}
			children := orderedVisibleChildCommands(child)
			if len(children) > 0 {
				walk(child, childGroup)
				continue
			}
			if childGroup != groupID {
				continue
			}
			path := strings.TrimPrefix(child.CommandPath(), root.Name()+" ")
			entries = append(entries, rootHelpEntry{path: path, short: child.Short})
		}
	}
	walk(root, "")
	return entries
}
