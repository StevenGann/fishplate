// Package version holds build metadata injected at link time with -ldflags.
package version

import (
	"fmt"
	"runtime"
)

// Set by the linker: -X github.com/StevenGann/fishplate/internal/version.Version=...
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns "fishplate <version> (<commit>, <date>, <go version>)".
func String() string {
	return fmt.Sprintf("fishplate %s (%s, %s, %s)", Version, Commit, Date, runtime.Version())
}
