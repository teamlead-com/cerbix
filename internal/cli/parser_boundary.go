package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

const allowExplicitFalseHelpAnnotation = "cerbix.io/allow-explicit-false-help"

type helpFlagIntent struct {
	sawTrue      bool
	sawFalse     bool
	sawInvalid   bool
	invalidToken string
}

func (i helpFlagIntent) absent() bool {
	return !i.sawTrue && !i.sawFalse && !i.sawInvalid
}

type parserBoundary struct {
	target     *cobra.Command
	targetArgs []string
	helpIntent helpFlagIntent
}

type parserBoundaryContextKey struct{}

func inspectParserBoundary(root *cobra.Command, args []string) (parserBoundary, error) {
	target := root
	remaining := args
	for hasVisibleSubcommands(target) {
		if len(remaining) == 0 || remaining[0] == "--" {
			break
		}
		token := remaining[0]
		if strings.HasPrefix(token, "-") {
			if isUnsupportedShorthand(token) {
				return parserBoundary{}, unsupportedShorthandError(target, token)
			}
			if !isHelpFlagToken(token) || containsDirectCommandToken(target, remaining[1:]) {
				return parserBoundary{}, commandPositionError(target, token)
			}
			break
		}
		child := parserBoundaryChild(target, token, target == root)
		if child == nil {
			return parserBoundary{}, commandPositionError(target, token)
		}
		target = child
		remaining = remaining[1:]
	}

	if target.Name() == "version" {
		if positional, found := firstVersionPositional(remaining); found {
			return parserBoundary{}, usageExit(fmt.Errorf("version: unexpected argument %q", positional))
		}
	}

	target.InitDefaultHelpFlag()
	intent, positionals, err := scanHelpFlagIntent(target, remaining)
	if err != nil {
		return parserBoundary{}, err
	}
	if target.Name() == "help" && target.Parent() == root {
		if _, err := resolveHelpPath(root, positionals); err != nil {
			return parserBoundary{}, err
		}
	} else if target.Args != nil && len(positionals) > 0 {
		if err := target.Args(target, positionals); err != nil {
			return parserBoundary{}, err
		}
	}
	return parserBoundary{
		target:     target,
		targetArgs: remaining,
		helpIntent: intent,
	}, nil
}

func parserBoundaryChild(parent *cobra.Command, name string, allowHelp bool) *cobra.Command {
	for _, child := range parent.Commands() {
		if child.Name() != name {
			continue
		}
		if child.Name() == "help" {
			if allowHelp {
				return child
			}
			return nil
		}
		if child.IsAvailableCommand() {
			return child
		}
		return nil
	}
	return nil
}

func containsDirectCommandToken(parent *cobra.Command, args []string) bool {
	for _, token := range args {
		if token == "--" {
			return false
		}
		if strings.HasPrefix(token, "-") {
			continue
		}
		if parserBoundaryChild(parent, token, false) != nil {
			return true
		}
	}
	return false
}

func commandPositionError(parent *cobra.Command, token string) error {
	if parent.Root() == parent && parent.Name() == "cerbix" {
		_, _ = fmt.Fprintf(parent.ErrOrStderr(), "unknown command %q\n", token)
		_ = writeRootHelp(parent, parent.ErrOrStderr())
		return printedExit(2)
	}
	return unknownSubcommandArgs(parent, []string{token})
}

func isHelpFlagToken(token string) bool {
	return token == "--help" || token == "-h" || strings.HasPrefix(token, "--help=") || strings.HasPrefix(token, "-h=")
}

func scanHelpFlagIntent(cmd *cobra.Command, args []string) (helpFlagIntent, []string, error) {
	var intent helpFlagIntent
	var positionals []string
	unknownFlag := false
	for i := 0; i < len(args); i++ {
		token := args[i]
		if token == "--" {
			if !unknownFlag {
				positionals = append(positionals, args[i+1:]...)
			}
			break
		}
		if isUnsupportedShorthand(token) {
			return intent, positionals, unsupportedShorthandError(cmd, token)
		}

		name, value, assigned, shorthand := splitFlagToken(token)
		if name == "" {
			if !unknownFlag && (token == "-" || !strings.HasPrefix(token, "-")) {
				positionals = append(positionals, token)
			}
			continue
		}
		if name == "help" || shorthand && name == "h" {
			if !assigned {
				intent.sawTrue = true
				continue
			}
			switch value {
			case "true":
				intent.sawTrue = true
			case "false":
				intent.sawFalse = true
			default:
				intent.sawInvalid = true
				if intent.invalidToken == "" {
					intent.invalidToken = token
				}
			}
			continue
		}

		flag := cmd.Flags().Lookup(name)
		if shorthand {
			flag = cmd.Flags().ShorthandLookup(name)
		}
		if flag == nil {
			unknownFlag = true // Cobra stops before interpreting later values as positionals.
		}
		if !assigned && flag != nil && flag.NoOptDefVal == "" && i+1 < len(args) {
			i++
		}
	}
	return intent, positionals, nil
}

func isUnsupportedShorthand(token string) bool {
	return strings.HasPrefix(token, "-") && !strings.HasPrefix(token, "--") && len(token) > 2 && !strings.HasPrefix(token, "-h=")
}

func unsupportedShorthandError(cmd *cobra.Command, token string) error {
	return usageExit(fmt.Errorf("%s: unsupported shorthand flag %q", commandDisplayPath(cmd), token))
}

func splitFlagToken(token string) (name, value string, assigned, shorthand bool) {
	switch {
	case strings.HasPrefix(token, "--") && len(token) > 2:
		body := token[2:]
		name, value, assigned = strings.Cut(body, "=")
		return name, value, assigned, false
	case strings.HasPrefix(token, "-") && len(token) == 2:
		return token[1:], "", false, true
	case strings.HasPrefix(token, "-") && len(token) >= 3 && token[2] == '=':
		return token[1:2], token[3:], true, true
	default:
		return "", "", false, false
	}
}

func allowsExplicitFalseHelp(cmd *cobra.Command) bool {
	return cmd.Annotations != nil && cmd.Annotations[allowExplicitFalseHelpAnnotation] == "true"
}

func executeRequestedHelp(root *cobra.Command, boundary parserBoundary) (bool, error) {
	intent := boundary.helpIntent
	if intent.absent() {
		return false, nil
	}
	if intent.sawFalse && !allowsExplicitFalseHelp(boundary.target) {
		return true, usageExit(fmt.Errorf("%s: explicitly false help is not allowed", commandDisplayPath(boundary.target)))
	}
	if intent.sawInvalid {
		return true, usageExit(fmt.Errorf("%s: invalid help assignment %q", commandDisplayPath(boundary.target), intent.invalidToken))
	}
	if !intent.sawTrue {
		return false, nil
	}

	target := boundary.target
	if err := target.ParseFlags(boundary.targetArgs); err != nil {
		return true, target.FlagErrorFunc()(target, err)
	}
	if target.Args != nil {
		if err := target.Args(target, target.Flags().Args()); err != nil {
			return true, err
		}
	}

	var err error
	if target.Name() == "help" && target.Parent() == root {
		return true, renderHelpPath(root, target.Flags().Args(), target.OutOrStdout())
	}
	if target == root {
		err = writeRootHelp(root, root.OutOrStdout())
	} else {
		err = writeCommandHelp(target, target.OutOrStdout())
	}
	if err != nil {
		return true, runtimeExit(fmt.Errorf("help: %w", err))
	}
	return true, nil
}

func renderHelpPath(root *cobra.Command, path []string, w io.Writer) error {
	target, err := resolveHelpPath(root, path)
	if err != nil {
		return err
	}
	if target == root {
		err = writeRootHelp(root, w)
	} else {
		err = writeCommandHelp(target, w)
	}
	if err != nil {
		return runtimeExit(fmt.Errorf("help: %w", err))
	}
	return nil
}

func resolveHelpPath(root *cobra.Command, path []string) (*cobra.Command, error) {
	target := root
	for _, name := range path {
		child := parserBoundaryChild(target, name, false)
		if child == nil {
			return nil, usageExit(fmt.Errorf("help: unknown command path %q", strings.Join(path, " ")))
		}
		target = child
	}
	return target, nil
}
