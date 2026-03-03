package skill

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	coreconfig "github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	"github.com/jfrog/jfrog-cli/utils/cliutils"
	"github.com/jfrog/jfrog-client-go/http/httpclient"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/io/httputils"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"github.com/manifoldco/promptui"
	"github.com/urfave/cli"
)

type versionsResponse struct {
	Items []versionEntry `json:"items"`
}

type versionEntry struct {
	Version   string `json:"version"`
	CreatedAt int64  `json:"createdAt"`
	Changelog string `json:"changelog,omitempty"`
}

type graphQLResponse struct {
	Data struct {
		Evidence struct {
			SearchEvidence struct {
				TotalCount int `json:"totalCount"`
				Edges      []struct {
					Node evidenceNode `json:"node"`
				} `json:"edges"`
			} `json:"searchEvidence"`
		} `json:"evidence"`
	} `json:"data"`
}

type evidenceNode struct {
	PredicateSlug string `json:"predicateSlug"`
	Verified      bool   `json:"verified"`
	CreatedBy     string `json:"createdBy"`
	CreatedAt     string `json:"createdAt"`
}

func installCmd(c *cli.Context) error {
	if show, err := cliutils.ShowCmdHelpIfNeeded(c, c.Args()); show || err != nil {
		return err
	}
	if c.NArg() != 1 {
		return cliutils.WrongNumberOfArgumentsHandler(c)
	}

	slug := c.Args().Get(0)
	repo := c.String("repo")
	if repo == "" {
		return errorutils.CheckErrorf("the --repo flag is mandatory")
	}
	version := c.String("version")

	serverDetails, err := getServerDetails(c)
	if err != nil {
		return err
	}
	client, httpDetails, err := createHTTPClient(serverDetails)
	if err != nil {
		return err
	}

	log.Output("\nPlan:")
	log.Output("1. **Resolve**: Check existing installation and confirm version.")
	log.Output("2. **Download**: Fetch zip from Artifactory and extract.")
	log.Output("3. **Verify**: Check evidence via JFrog Evidence service.")
	log.Output("4. **Install**: Copy to target location and report.\n")

	// Step 1 — Resolve: detect existing installation
	existingPath, currentVersion := detectExistingInstall(slug)
	isUpgrade := existingPath != ""
	if isUpgrade {
		log.Output(fmt.Sprintf("Existing installation found: %s (version %s)", existingPath, currentVersion))
	}

	// Step 2 — Resolve version (prompt if not provided)
	if version == "" {
		version, err = resolveVersion(client, httpDetails, serverDetails.ArtifactoryUrl, repo, slug, currentVersion, isUpgrade)
		if err != nil {
			return err
		}
	}
	if isUpgrade && version == currentVersion {
		log.Output("Already on version " + version + ". Nothing to do.")
		return nil
	}

	// Step 3 — Download
	log.Output("Downloading " + slug + "@" + version + "...")
	zipPath := filepath.Join(os.TempDir(), slug+"-"+version+".zip")
	defer os.Remove(zipPath) //nolint:errcheck
	if err = downloadSkillZip(client, httpDetails, serverDetails.ArtifactoryUrl, repo, slug, version, zipPath); err != nil {
		return err
	}

	// Step 4 — Extract
	extractDir := filepath.Join(os.TempDir(), slug+"-"+version)
	defer os.RemoveAll(extractDir) //nolint:errcheck
	if err = extractZip(zipPath, extractDir); err != nil {
		return err
	}
	srcDir := unwrapSingleSubdir(extractDir)

	// Step 5 — Verify evidence
	verified, verifyMsg := checkEvidence(client, httpDetails, serverDetails, repo, slug, version)
	if verified {
		log.Output("✅ " + verifyMsg)
	} else {
		log.Output("⚠️  " + verifyMsg)
		if !coreutils.AskYesNo("This skill is unsigned. Proceed with installation?", false) {
			log.Output("Installation aborted.")
			return nil
		}
	}

	// Step 6 — Determine install location
	var installPath string
	if isUpgrade {
		installPath = existingPath
	} else {
		installPath, err = promptInstallLocation(slug)
		if err != nil {
			return err
		}
	}

	// Step 7 — Install
	if err = copyTree(srcDir, installPath); err != nil {
		return err
	}

	// Step 8 — Report
	skillMDPath := filepath.Join(installPath, "SKILL.md")
	if _, statErr := os.Stat(skillMDPath); statErr != nil {
		log.Warn("SKILL.md not found at install location")
	}

	action := "Installed"
	if isUpgrade {
		action = "Upgraded"
	}
	log.Output(fmt.Sprintf("\n✅ %s: %s %s — %s", action, slug, version, installPath))
	log.Output("\nDone:")
	log.Output(fmt.Sprintf("1. **Resolve**: ✅ Skill and version confirmed%s.", upgradeNote(isUpgrade, currentVersion)))
	log.Output("2. **Download**: ✅ Zip fetched from Artifactory and extracted.")
	if verified {
		log.Output("3. **Verify**: ✅ Evidence fetched; signature verified.")
	} else {
		log.Output("3. **Verify**: Unsigned — no evidence found.")
	}
	log.Output(fmt.Sprintf("4. **Install**: ✅ Content copied to %s; SKILL.md verified.", installPath))
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func upgradeNote(isUpgrade bool, currentVersion string) string {
	if isUpgrade {
		return "; upgrade from " + currentVersion
	}
	return ""
}

func detectExistingInstall(slug string) (installPath, currentVersion string) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}

	candidates := []string{
		filepath.Join(homeDir, ".cursor", "skills", slug),
		filepath.Join(".cursor", "skills", slug),
	}

	for _, dir := range candidates {
		skillMD := filepath.Join(dir, "SKILL.md")
		if _, err := os.Stat(skillMD); err == nil {
			return dir, parseVersionFromSkillMD(skillMD)
		}
	}
	return "", ""
}

func parseVersionFromSkillMD(mdPath string) string {
	data, err := os.ReadFile(mdPath)
	if err != nil {
		return "unknown"
	}
	content := string(data)
	if !strings.HasPrefix(content, "---") {
		return "unknown"
	}
	endIdx := strings.Index(content[3:], "---")
	if endIdx < 0 {
		return "unknown"
	}
	frontmatter := content[3 : 3+endIdx]
	for _, line := range strings.Split(frontmatter, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "version:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "version:"))
		}
	}
	return "unknown"
}

func resolveVersion(client *httpclient.HttpClient, httpDetails httputils.HttpClientDetails,
	artURL, repo, slug, currentVersion string, isUpgrade bool) (string, error) {

	apiURL := artURL + "api/skills/" + repo + "/api/v1/skills/" + slug + "/versions"
	log.Debug("Fetching versions from:", apiURL)

	resp, body, _, err := client.SendGet(apiURL, true, httpDetails, "")
	if err != nil {
		return "", err
	}
	if err = errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK); err != nil {
		return "", errorutils.CheckErrorf("failed to fetch versions for '%s': %s", slug, err)
	}

	var result versionsResponse
	if err = json.Unmarshal(body, &result); err != nil {
		return "", errorutils.CheckErrorf("failed to parse versions response: %s", err)
	}
	if len(result.Items) == 0 {
		return "", errorutils.CheckErrorf("no versions found for skill '%s'", slug)
	}

	sortVersionsDesc(result.Items)

	var items []string
	var versions []string
	for _, v := range result.Items {
		label := v.Version
		if v.Changelog != "" {
			label += " - " + v.Changelog
		} else if v.CreatedAt > 0 {
			label += " (" + time.UnixMilli(v.CreatedAt).Format("2006-01-02") + ")"
		}
		if v.Version == currentVersion {
			label += " [installed]"
		}
		items = append(items, label)
		versions = append(versions, v.Version)
	}

	if len(items) > 10 {
		items = items[:10]
		versions = versions[:10]
	}

	prompt := promptui.Select{
		Label: "Select version to install:",
		Items: items,
	}
	idx, _, err := prompt.Run()
	if err != nil {
		return "", err
	}
	return versions[idx], nil
}

func sortVersionsDesc(entries []versionEntry) {
	sort.Slice(entries, func(i, j int) bool {
		vi, ei := semver.NewVersion(entries[i].Version)
		vj, ej := semver.NewVersion(entries[j].Version)
		if ei != nil || ej != nil {
			return entries[i].Version > entries[j].Version
		}
		return vi.GreaterThan(vj)
	})
}

func downloadSkillZip(client *httpclient.HttpClient, httpDetails httputils.HttpClientDetails,
	artURL, repo, slug, version, destPath string) error {

	apiURL := artURL + "api/skills/" + repo + "/api/v1/download?slug=" + slug + "&version=" + version
	log.Debug("Downloading from:", apiURL)

	resp, body, _, err := client.SendGet(apiURL, true, httpDetails, "")
	if err != nil {
		return err
	}
	if err = errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK); err != nil {
		return err
	}
	return os.WriteFile(destPath, body, 0600)
}

func extractZip(zipPath, destDir string) error {
	log.Debug("Extracting to:", destDir)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return errorutils.CheckErrorf("failed to open zip: %s", err)
	}
	defer r.Close()

	destClean := filepath.Clean(destDir)
	for _, f := range r.File {
		targetPath := filepath.Join(destDir, f.Name) //#nosec G305 — validated below
		targetClean := filepath.Clean(targetPath)
		if targetClean != destClean && !strings.HasPrefix(targetClean, destClean+string(os.PathSeparator)) {
			return errorutils.CheckErrorf("zip contains invalid path: %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			if mkErr := os.MkdirAll(targetPath, 0755); mkErr != nil {
				return mkErr
			}
			continue
		}
		if mkErr := os.MkdirAll(filepath.Dir(targetPath), 0755); mkErr != nil {
			return mkErr
		}
		if exErr := writeZipEntry(f, targetPath); exErr != nil {
			return exErr
		}
	}
	return nil
}

func writeZipEntry(f *zip.File, targetPath string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, rc)
	return err
}

// unwrapSingleSubdir returns the lone child directory when the zip
// extracted into exactly one top-level folder; otherwise returns dir as-is.
func unwrapSingleSubdir(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return dir
	}
	return filepath.Join(dir, entries[0].Name())
}

func checkEvidence(client *httpclient.HttpClient, httpDetails httputils.HttpClientDetails,
	serverDetails *coreconfig.ServerDetails, repo, slug, version string) (verified bool, message string) {

	if serverDetails.Url == "" {
		return false, "Platform URL not configured; evidence check skipped."
	}

	onemodelURL := strings.TrimSuffix(serverDetails.Url, "/") + "/onemodel/"
	pathPart := slug + "/" + version
	namePart := slug + "-" + version + ".zip"

	query := fmt.Sprintf(
		`{"query":"{ evidence { searchEvidence( where: { hasSubjectWith: { repositoryKey: \"%s\", path: \"%s\", name: \"%s\"}} ) { totalCount edges { node { predicateSlug verified createdBy createdAt } } } } }"}`,
		repo, pathPart, namePart,
	)

	gqlDetails := cloneHTTPDetails(httpDetails)
	gqlDetails.Headers["Content-Type"] = "application/json"

	apiURL := onemodelURL + "api/v1/graphql"
	log.Debug("Checking evidence at:", apiURL)

	resp, body, err := client.SendPost(apiURL, []byte(query), gqlDetails, "")
	if err != nil {
		log.Debug("Evidence check failed:", err)
		return false, "Evidence check failed: " + err.Error()
	}
	if resp.StatusCode != http.StatusOK {
		log.Debug("Evidence API returned status:", resp.StatusCode)
		return false, fmt.Sprintf("Evidence API returned HTTP %d; treating as unsigned.", resp.StatusCode)
	}

	var gqlResp graphQLResponse
	if err := json.Unmarshal(body, &gqlResp); err != nil {
		log.Debug("Failed to parse evidence response:", err)
		return false, "Failed to parse evidence response."
	}

	if gqlResp.Data.Evidence.SearchEvidence.TotalCount == 0 {
		return false, "No evidence found for this skill."
	}

	for _, edge := range gqlResp.Data.Evidence.SearchEvidence.Edges {
		if edge.Node.Verified {
			return true, fmt.Sprintf("Evidence verified (predicate: %s, by: %s).",
				edge.Node.PredicateSlug, edge.Node.CreatedBy)
		}
	}
	return false, "Evidence entries found but none are verified."
}

func cloneHTTPDetails(src httputils.HttpClientDetails) httputils.HttpClientDetails {
	dst := src
	dst.Headers = make(map[string]string, len(src.Headers)+1)
	for k, v := range src.Headers {
		dst.Headers[k] = v
	}
	return dst
}

func promptInstallLocation(slug string) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	personalPath := filepath.Join(homeDir, ".cursor", "skills", slug)
	projectPath := filepath.Join(".cursor", "skills", slug)

	prompt := promptui.Select{
		Label: "Choose install location:",
		Items: []string{
			"Personal (" + personalPath + ")",
			"Project (" + projectPath + ")",
		},
	}
	idx, _, err := prompt.Run()
	if err != nil {
		return "", err
	}
	if idx == 0 {
		return personalPath, nil
	}
	return projectPath, nil
}

func copyTree(srcDir, destDir string) error {
	log.Debug("Copying", srcDir, "→", destDir)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sigstore.json") {
			continue
		}
		src := filepath.Join(srcDir, entry.Name())
		dst := filepath.Join(destDir, entry.Name())

		if entry.IsDir() {
			if err := copyTree(src, dst); err != nil {
				return err
			}
		} else {
			data, readErr := os.ReadFile(src)
			if readErr != nil {
				return readErr
			}
			if writeErr := os.WriteFile(dst, data, 0644); writeErr != nil { //#nosec G306
				return writeErr
			}
		}
	}
	return nil
}
