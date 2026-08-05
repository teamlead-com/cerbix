package buildinfo

import (
	"runtime"
	"testing"
)

func TestCurrentReturnsInjectedBuildMetadata(t *testing.T) {
	oldVersion := Version
	oldCommit := Commit
	t.Cleanup(func() {
		Version = oldVersion
		Commit = oldCommit
	})
	Version = "v1.2.3"
	Commit = "deadbeef"

	info := Current()
	if info.Version != "v1.2.3" || info.Commit != "deadbeef" || info.GoVersion != runtime.Version() {
		t.Fatalf("Current() = %#v", info)
	}
}
