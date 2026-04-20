package version

import "strings"

var (
	Current   = "0.1.0-dev"
	Commit    = "dev"
	ImageTag  = "dev"
	BuildTime = ""
)

func CleanVersion() string {
	return strings.TrimSpace(Current)
}

func CleanCommit() string {
	return strings.TrimSpace(Commit)
}

func CleanImageTag() string {
	return strings.TrimSpace(ImageTag)
}

func ShortCommit() string {
	commit := CleanCommit()
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
