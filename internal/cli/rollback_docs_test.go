package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validateCLIRollbackDocument(documentPath, goldenPath string) error {
	body, err := os.ReadFile(documentPath)
	if err != nil {
		return fmt.Errorf("read CLI rollback procedure: %w", err)
	}
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		return fmt.Errorf("read reviewed CLI rollback golden: %w", err)
	}
	if strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(string(body)) != string(golden) {
		return fmt.Errorf("CLI rollback procedure differs from the reviewed whole-file golden")
	}
	return nil
}

func TestCLIRollbackWholeFileMatchesGolden(t *testing.T) {
	document := filepath.Join("..", "..", "docs", "specs", "cross-cli-rollback.md")
	if err := validateCLIRollbackDocument(document, "rollback_golden_test.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestCLIRollbackWholeFileRejectsAnyUnreviewedEdit(t *testing.T) {
	document := filepath.Join("..", "..", "docs", "specs", "cross-cli-rollback.md")
	body, err := os.ReadFile(document)
	if err != nil {
		t.Fatal(err)
	}
	canonical := string(body)
	for _, tc := range []struct {
		name, changed string
	}{
		{"append instruction", canonical + "Revert only one commit.\n"},
		{"append second section", canonical + "\n## Alternative rollback\nRevert only one commit.\n"},
		{"remove approval", strings.Replace(canonical, "Do not perform the restoration without separate owner approval.\n", "", 1)},
		{"replace instruction", strings.Replace(canonical, "**not** a revert of one commit", "**now** a revert of one commit", 1)},
		{"remove heading", strings.TrimPrefix(canonical, "# Cobra CLI rollback — canonical recovery procedure\n\n")},
		{"remove final newline", strings.TrimSuffix(canonical, "\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.changed == canonical {
				t.Fatal("fixture did not change the procedure")
			}
			copyPath := filepath.Join(t.TempDir(), "rollback.md")
			if err := os.WriteFile(copyPath, []byte(tc.changed), 0600); err != nil {
				t.Fatal(err)
			}
			if err := validateCLIRollbackDocument(copyPath, "rollback_golden_test.txt"); err == nil {
				t.Error("unreviewed whole-file edit left rollback guard GREEN")
			}
		})
	}
	for _, lineEnding := range []struct{ name, changed string }{
		{"CRLF", strings.ReplaceAll(canonical, "\n", "\r\n")},
		{"lone CR", strings.ReplaceAll(canonical, "\n", "\r")},
	} {
		t.Run(lineEnding.name, func(t *testing.T) {
			copyPath := filepath.Join(t.TempDir(), "rollback.md")
			if err := os.WriteFile(copyPath, []byte(lineEnding.changed), 0600); err != nil {
				t.Fatal(err)
			}
			if err := validateCLIRollbackDocument(copyPath, "rollback_golden_test.txt"); err != nil {
				t.Errorf("normalized line endings changed the approved procedure: %v", err)
			}
		})
	}
	golden, err := os.ReadFile("rollback_golden_test.txt")
	if err != nil {
		t.Fatal(err)
	}
	changedGolden := filepath.Join(t.TempDir(), "changed-golden.txt")
	if err := os.WriteFile(changedGolden, append(golden, []byte("Revert just one commit.\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateCLIRollbackDocument(document, changedGolden); err == nil {
		t.Error("changed golden must not be inferred from the procedure")
	}
}
