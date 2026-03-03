package skill

import (
	"encoding/json"
	"fmt"
	"net/http"

	corecommon "github.com/jfrog/jfrog-cli-core/v2/docs/common"
	coreconfig "github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-cli/docs/general/skill/install"
	"github.com/jfrog/jfrog-cli/docs/general/skill/search"
	"github.com/jfrog/jfrog-cli/utils/cliutils"
	"github.com/jfrog/jfrog-client-go/http/httpclient"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/io/httputils"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"github.com/urfave/cli"
)

type searchResponse struct {
	Results []skillEntry `json:"results"`
	Total   int          `json:"total"`
}

type skillEntry struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

func GetCommands() []cli.Command {
	return cliutils.GetSortedCommands(cli.CommandsByName{
		{
			Name:         "search",
			Flags:        cliutils.GetCommandFlags(cliutils.SkillSearch),
			Usage:        search.GetDescription(),
			HelpName:     corecommon.CreateUsage("skill search", search.GetDescription(), search.Usage),
			BashComplete: corecommon.CreateBashCompletionFunc(),
			Action:       searchCmd,
		},
		{
			Name:         "install",
			Flags:        cliutils.GetCommandFlags(cliutils.SkillInstall),
			Usage:        install.GetDescription(),
			HelpName:     corecommon.CreateUsage("skill install", install.GetDescription(), install.Usage),
			UsageText:    install.GetArguments(),
			BashComplete: corecommon.CreateBashCompletionFunc(),
			Action:       installCmd,
		},
	})
}

func searchCmd(c *cli.Context) error {
	if show, err := cliutils.ShowCmdHelpIfNeeded(c, c.Args()); show || err != nil {
		return err
	}

	query := c.String("query")
	if query == "" {
		return errorutils.CheckErrorf("the --query flag is mandatory")
	}
	repo := c.String("repo")
	if repo == "" {
		return errorutils.CheckErrorf("the --repo flag is mandatory")
	}

	serverDetails, err := getServerDetails(c)
	if err != nil {
		return err
	}
	client, httpDetails, err := createHTTPClient(serverDetails)
	if err != nil {
		return err
	}

	apiURL := serverDetails.ArtifactoryUrl + "api/skills/" + repo + "/api/v1/search?q=" + query + "&limit=10"
	log.Debug("Sending search request to:", apiURL)

	resp, body, _, err := client.SendGet(apiURL, true, httpDetails, "")
	if err != nil {
		return err
	}
	if err = errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK); err != nil {
		return err
	}

	var result searchResponse
	if err = json.Unmarshal(body, &result); err != nil {
		return errorutils.CheckErrorf("failed to parse search response: %s", err)
	}

	if len(result.Results) == 0 {
		log.Output("No skills found for query: " + query)
		return nil
	}

	for _, s := range result.Results {
		log.Output(fmt.Sprintf("  %s@%s - %s", s.Name, s.Version, s.Description))
	}
	return nil
}

// Shared helpers used by both search and install commands.

func getServerDetails(c *cli.Context) (*coreconfig.ServerDetails, error) {
	serverID := c.String("server-id")
	return coreconfig.GetSpecificConfig(serverID, true, true)
}

func createHTTPClient(serverDetails *coreconfig.ServerDetails) (*httpclient.HttpClient, httputils.HttpClientDetails, error) {
	auth, err := serverDetails.CreateArtAuthConfig()
	if err != nil {
		return nil, httputils.HttpClientDetails{}, err
	}
	details := auth.CreateHttpClientDetails()
	client, err := httpclient.ClientBuilder().Build()
	if err != nil {
		return nil, httputils.HttpClientDetails{}, err
	}
	return client, details, nil
}
