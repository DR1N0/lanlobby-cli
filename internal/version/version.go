// Package version exposes build metadata injected with Go linker flags.
package version

import "runtime"

var (
	gitVersion   = "v0.0.1"
	gitCommit    = "unknown"
	gitTreeState = "unknown"
	buildDate    = "unknown"
)

type Info struct {
	GitVersion   string
	GitCommit    string
	GitTreeState string
	BuildDate    string
	GoVersion    string
	Platform     string
}

func Get() Info {
	return Info{
		GitVersion:   gitVersion,
		GitCommit:    gitCommit,
		GitTreeState: gitTreeState,
		BuildDate:    buildDate,
		GoVersion:    runtime.Version(),
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
	}
}
