package search

var Usage = []string{"skill search --query=<query> --repo=<repo-key>"}

func GetDescription() string {
	return "Search for skills in an Artifactory repository."
}
