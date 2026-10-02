package cli

import (
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestProductionCLIHasOneParserDependencyBoundary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "flag."+"NewFlagSet") {
			t.Errorf("%s contains a native flag parser", name)
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, body, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case path == "flag":
				t.Errorf("%s imports the standard-library flag parser", name)
			case path == "github.com/spf13/pflag":
				t.Errorf("%s imports pflag directly; Cobra must be the only direct parser dependency", name)
			case strings.Contains(path, "spf13/viper"):
				t.Errorf("%s imports prohibited Viper package %q", name, path)
			}
		}
	}

	mod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(mod)
	if !strings.Contains(text, "\tgithub.com/spf13/cobra v1.10.2\n") {
		t.Error("go.mod does not pin direct Cobra v1.10.2")
	}
	if strings.Contains(text, "github.com/spf13/"+"viper") {
		t.Error("go.mod contains prohibited Viper dependency")
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "github.com/spf13/pflag") && !strings.Contains(line, "// indirect") {
			t.Errorf("pflag is not transitive in go.mod: %q", line)
		}
	}
}

func TestREADMEListsChangeRecordTimeout(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	start := strings.Index(text, "cerbix change record --project")
	if start < 0 {
		t.Fatal("README change record invocation not found")
	}
	end := strings.Index(text[start:], "cerbix version")
	if end < 0 {
		t.Fatal("README version boundary not found")
	}
	changeInvocation := text[start : start+end]
	if !strings.Contains(changeInvocation, "[--json] [--timeout 10s]") {
		t.Fatal("README change record invocation omits --timeout 10s")
	}
}

func TestRootConstructionDoesNotMutateGlobalCobraSorting(t *testing.T) {
	original := cobra.EnableCommandSorting
	t.Cleanup(func() { cobra.EnableCommandSorting = original })
	cobra.EnableCommandSorting = true
	_ = newRootCommand(io.Discard, io.Discard)
	if !cobra.EnableCommandSorting {
		t.Fatal("newRootCommand mutated cobra.EnableCommandSorting")
	}
}

func TestRootCatalogueExcludesHiddenAndUnavailableCommands(t *testing.T) {
	root := newRootCommand(io.Discard, io.Discard)
	addOrderedCommands(root,
		&cobra.Command{Use: "hidden", Short: "hidden", GroupID: rootGroupRuntime, Hidden: true, Run: func(*cobra.Command, []string) {}},
		&cobra.Command{Use: "unavailable", Short: "unavailable", GroupID: rootGroupRuntime},
	)
	var out strings.Builder
	if err := writeRootHelp(root, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "hidden") || strings.Contains(out.String(), "unavailable") {
		t.Fatalf("root catalogue exposed hidden/unavailable command:\n%s", out.String())
	}
}

func TestRootTreeContainsTheExactGroupedRunnableLeafSet(t *testing.T) {
	root := newRootCommand(io.Discard, io.Discard)
	want := map[string]string{
		"serve":                  rootGroupRuntime,
		"migrate":                rootGroupRuntime,
		"reencrypt":              rootGroupRuntime,
		"adopt-fact-month":       rootGroupMaintenance,
		"enqueue-service-repair": rootGroupMaintenance,
		"gate check":             rootGroupCICD,
		"change record":          rootGroupCICD,
		"version":                rootGroupOther,
	}
	got := make(map[string]string)
	var walk func(*cobra.Command, string)
	walk = func(parent *cobra.Command, inheritedGroup string) {
		for _, child := range parent.Commands() {
			if child.Hidden || child.Name() == "help" {
				continue
			}
			group := inheritedGroup
			if parent == root {
				if child.GroupID == "" {
					t.Errorf("top-level command %q has no group", child.Name())
				}
				group = child.GroupID
			}
			children := visibleChildCommands(child)
			if len(children) == 0 {
				path := strings.TrimPrefix(child.CommandPath(), "cerbix ")
				got[path] = group
				continue
			}
			walk(child, group)
		}
	}
	walk(root, "")
	if len(got) != len(want) {
		t.Fatalf("runnable leaf set = %v, want %v", got, want)
	}
	for path, group := range want {
		if got[path] != group {
			t.Errorf("leaf %q group = %q, want %q", path, got[path], group)
		}
	}
}

func visibleChildCommands(cmd *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, child := range cmd.Commands() {
		if !child.Hidden && child.Name() != "help" {
			out = append(out, child)
		}
	}
	return out
}
