package install

var Usage = []string{"skill install <slug> --repo=<repo-key> [--version=<version>]"}

func GetDescription() string {
	return "Download and install a skill from an Artifactory repository."
}

func GetArguments() string {
	return `	slug
		The skill slug to install.`
}
