// Package version holds build metadata injected at link time:
//
//	go build -ldflags "-X github.com/bughatti/transcoder/internal/version.Version=v1.2.3"
package version

var (
	// Version is the semantic version or git describe output.
	Version = "dev"
	// Commit is the short git SHA.
	Commit = ""
	// Date is the build timestamp (RFC3339).
	Date = ""
)

// String renders "v1.2.3 (abc1234, 2026-01-01T00:00:00Z)" or just "dev".
func String() string {
	s := Version
	if Commit != "" {
		s += " (" + Commit
		if Date != "" {
			s += ", " + Date
		}
		s += ")"
	}
	return s
}
