package version

import (
	"fmt"
)

var (
	Version = "dev"
	Commit  = ""
	BuiltAt = ""
)

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	BuiltAt string `json:"built_at,omitempty"`
}

func Current() Info {
	return Info{
		Version: Version,
		Commit:  ShortCommit(Commit),
		BuiltAt: BuiltAt,
	}
}

func ShortCommit(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}

func String() string {
	info := Current()
	out := info.Version
	if info.Commit != "" {
		out += " " + info.Commit
	}
	if info.BuiltAt != "" {
		out += " " + info.BuiltAt
	}
	return out
}

func Print(component string) {
	if component == "" {
		component = "mtr"
	}
	fmt.Printf("%s %s\n", component, String())
}
