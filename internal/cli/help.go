package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	helpAnnotationRequired    = "cerbix.io/required"
	helpAnnotationPlaceholder = "cerbix.io/placeholder"
	helpAnnotationFlagOrder   = "cerbix.io/flag-order"
	helpAnnotationEnvironment = "cerbix.io/environment"
	helpAnnotationExitCodes   = "cerbix.io/exit-codes"
)

func addStringFlag(cmd *cobra.Command, target *string, name, defaultValue, placeholder, usage string, required bool) {
	cmd.Flags().StringVar(target, name, defaultValue, usage)
	recordFlagMetadata(cmd, name, placeholder, required)
}

func addBoolFlag(cmd *cobra.Command, target *bool, name string, defaultValue bool, usage string) {
	cmd.Flags().BoolVar(target, name, defaultValue, usage)
	recordFlagMetadata(cmd, name, "", false)
}

func addDurationFlag(cmd *cobra.Command, target *time.Duration, name string, defaultValue time.Duration, usage string) {
	cmd.Flags().DurationVar(target, name, defaultValue, usage)
	recordFlagMetadata(cmd, name, "duration", false)
}

func recordFlagMetadata(cmd *cobra.Command, name, placeholder string, required bool) {
	flag := cmd.Flags().Lookup(name)
	if flag == nil {
		panic("help metadata flag is not registered: " + name)
	}
	if flag.Annotations == nil {
		flag.Annotations = make(map[string][]string)
	}
	flag.Annotations[helpAnnotationPlaceholder] = []string{placeholder}
	if required {
		flag.Annotations[helpAnnotationRequired] = []string{"true"}
	}
	if cmd.Annotations == nil {
		cmd.Annotations = make(map[string]string)
	}
	order := commandFlagNames(cmd)
	order = append(order, name)
	cmd.Annotations[helpAnnotationFlagOrder] = strings.Join(order, ",")
}

func commandFlagNames(cmd *cobra.Command) []string {
	if cmd.Annotations == nil || cmd.Annotations[helpAnnotationFlagOrder] == "" {
		return nil
	}
	return strings.Split(cmd.Annotations[helpAnnotationFlagOrder], ",")
}

func flagIsRequired(cmd *cobra.Command, name string) bool {
	flag := cmd.Flags().Lookup(name)
	if flag == nil {
		return false
	}
	values := flag.Annotations[helpAnnotationRequired]
	return len(values) > 0 && values[0] == "true"
}

func flagPlaceholder(cmd *cobra.Command, name string) string {
	flag := cmd.Flags().Lookup(name)
	if flag == nil {
		return ""
	}
	values := flag.Annotations[helpAnnotationPlaceholder]
	if len(values) > 0 && values[0] != "" {
		return values[0]
	}
	return flag.Value.Type()
}

func setHelpSection(cmd *cobra.Command, annotation, body string) {
	if cmd.Annotations == nil {
		cmd.Annotations = make(map[string]string)
	}
	cmd.Annotations[annotation] = body
}

func newHelpCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:                   "help [command]",
		Short:                 "Show command help.",
		Hidden:                true,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return renderHelpPath(root, args, cmd.OutOrStdout())
		},
	}
}

func writeCommandHelp(cmd *cobra.Command, w io.Writer) error {
	if _, err := fmt.Fprintf(w, "%s — %s\n", cmd.CommandPath(), cmd.Short); err != nil {
		return err
	}
	if cmd.Long != "" {
		if _, err := fmt.Fprintf(w, "\n%s\n", cmd.Long); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "\nUsage:\n  %s\n", commandUsageLine(cmd)); err != nil {
		return err
	}
	if err := writeSubcommandHelpSection(w, cmd); err != nil {
		return err
	}

	cmd.InitDefaultHelpFlag()
	var required, options []string
	for _, name := range commandFlagNames(cmd) {
		flag := cmd.Flags().Lookup(name)
		if flag == nil || flag.Hidden {
			continue
		}
		if flagIsRequired(cmd, name) {
			required = append(required, name)
		} else {
			options = append(options, name)
		}
	}
	if helpFlag := cmd.Flags().Lookup("help"); helpFlag != nil && !helpFlag.Hidden {
		options = append(options, "help")
	}
	if err := writeFlagHelpSection(w, "Required flags:", cmd, required); err != nil {
		return err
	}
	if err := writeFlagHelpSection(w, "Options:", cmd, options); err != nil {
		return err
	}
	if err := writeTextHelpSection(w, "Environment:", cmd.Annotations[helpAnnotationEnvironment]); err != nil {
		return err
	}
	if err := writeTextHelpSection(w, "Exit codes:", cmd.Annotations[helpAnnotationExitCodes]); err != nil {
		return err
	}
	if cmd.Example != "" {
		if _, err := fmt.Fprintln(w, "\nExamples:"); err != nil {
			return err
		}
		for _, line := range strings.Split(cmd.Example, "\n") {
			if _, err := fmt.Fprintf(w, "  %s\n", line); err != nil {
				return err
			}
		}
	}
	return nil
}

func commandUsageLine(cmd *cobra.Command) string {
	path := cmd.CommandPath()
	if hasVisibleSubcommands(cmd) {
		return path + " <command> [flags]"
	}
	for _, name := range commandFlagNames(cmd) {
		flag := cmd.Flags().Lookup(name)
		if flag == nil || flag.Hidden {
			continue
		}
		token := "--" + name
		if flag.Value.Type() != "bool" {
			token += " <" + flagPlaceholder(cmd, name) + ">"
		}
		if flagIsRequired(cmd, name) {
			path += " " + token
		} else {
			path += " [" + token + "]"
		}
	}
	return path
}

func hasVisibleSubcommands(cmd *cobra.Command) bool {
	return len(orderedVisibleChildCommands(cmd)) > 0
}

func writeSubcommandHelpSection(w io.Writer, cmd *cobra.Command) error {
	children := orderedVisibleChildCommands(cmd)
	if len(children) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\nCommands:"); err != nil {
		return err
	}
	width := 0
	for _, child := range children {
		if len(child.Name()) > width {
			width = len(child.Name())
		}
	}
	for _, child := range children {
		if _, err := fmt.Fprintf(w, "  %-*s  %s\n", width, child.Name(), child.Short); err != nil {
			return err
		}
	}
	return nil
}

func writeFlagHelpSection(w io.Writer, title string, cmd *cobra.Command, names []string) error {
	if len(names) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n%s\n", title); err != nil {
		return err
	}
	labels := make([]string, len(names))
	width := 0
	for i, name := range names {
		labels[i] = helpFlagLabel(cmd, name)
		if len(labels[i]) > width {
			width = len(labels[i])
		}
	}
	for i, name := range names {
		flag := cmd.Flags().Lookup(name)
		usage := flag.Usage
		if name == "help" {
			usage = "help for " + commandDisplayPath(cmd)
		}
		if !flagIsRequired(cmd, name) && name != "help" && flag.DefValue != "" && flag.DefValue != "false" {
			value := flag.DefValue
			if flag.Value.Type() == "string" {
				value = strconv.Quote(value)
			}
			usage += " (default " + value + ")"
		}
		if _, err := fmt.Fprintf(w, "  %-*s  %s\n", width, labels[i], usage); err != nil {
			return err
		}
	}
	return nil
}

func helpFlagLabel(cmd *cobra.Command, name string) string {
	flag := cmd.Flags().Lookup(name)
	label := "--" + flag.Name
	if flag.Shorthand != "" {
		label = "-" + flag.Shorthand + ", " + label
	}
	if flag.Value.Type() == "bool" {
		return label
	}
	return label + " " + flag.Value.Type()
}

func writeTextHelpSection(w io.Writer, title, body string) error {
	if body == "" {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n%s\n", title); err != nil {
		return err
	}
	for _, line := range strings.Split(body, "\n") {
		if _, err := fmt.Fprintf(w, "  %s\n", line); err != nil {
			return err
		}
	}
	return nil
}
