package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/google/go-github/v55/github"
	"golang.org/x/oauth2"
	"gopkg.in/yaml.v3"
)

// Configuration - set maxRepos to 0 to disable limit
const maxRepos = 0 // Change to 0 to scan all repositories
const enableLimit = maxRepos > 0

type ActionInfo struct {
	Repos            map[string]struct{}
	UsesNpm          bool
	IsInfected       bool
	InfectedPackages []string
	Analyzed         bool
}

func main() {
	ctx := context.Background()
	client := createGitHubClient(ctx)

	org := getRequiredEnv("GITHUB_ORG")

	fmt.Printf("Scanning repositories in organization: %s\n", org)

	usesRepos := scanRepositories(ctx, client, org)
	analyzeActions(ctx, client, usesRepos)
	printResults(usesRepos)
}

func createGitHubClient(ctx context.Context) *github.Client {
	// I used a classic token with repo scope
	token := getRequiredEnv("GITHUB_TOKEN")

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)
	return github.NewClient(tc)
}

func getRequiredEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		log.Fatalf("%s environment variable is not set", key)
	}
	return value
}

func scanRepositories(ctx context.Context, client *github.Client, org string) map[string]*ActionInfo {
	usesRepos := make(map[string]*ActionInfo)

	opt := &github.RepositoryListByOrgOptions{
		ListOptions: github.ListOptions{PerPage: 50},
	}

	repoCount := 0

	for {
		repos, resp, err := client.Repositories.ListByOrg(ctx, org, opt)
		if err != nil {
			log.Fatalf("Error listing repositories: %v", err)
		}

		for _, repo := range repos {
			if shouldStopScanning(repoCount) {
				fmt.Printf("Reached maximum of %d repositories for testing.\n", maxRepos)
				return usesRepos
			}

			repoName := repo.GetName()
			logRepoProgress(repoCount, repoName)
			repoCount++

			processRepository(ctx, client, org, repoName, usesRepos)
		}

		if shouldStopScanning(repoCount) || resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}

	return usesRepos
}

func shouldStopScanning(repoCount int) bool {
	return enableLimit && repoCount >= maxRepos
}

func logRepoProgress(repoCount int, repoName string) {
	if enableLimit {
		fmt.Printf("Processing repository %d/%d: %s\n", repoCount+1, maxRepos, repoName)
	} else {
		fmt.Printf("Processing repository: %s\n", repoName)
	}
}

func processRepository(ctx context.Context, client *github.Client, org, repoName string, usesRepos map[string]*ActionInfo) {
	_, contents, _, err := client.Repositories.GetContents(ctx, org, repoName, ".github/workflows", nil)
	if err != nil {
		if _, ok := err.(*github.ErrorResponse); ok {
			return // Directory doesn't exist, skip
		}
		log.Printf("Error getting contents of .github/workflows in %s: %v", repoName, err)
		return
	}

	for _, content := range contents {
		if isWorkflowFile(content) {
			processWorkflowFile(ctx, client, org, repoName, content, usesRepos)
		}
	}
}

func isWorkflowFile(content *github.RepositoryContent) bool {
	return content.GetType() == "file" &&
		(strings.HasSuffix(content.GetName(), ".yml") || strings.HasSuffix(content.GetName(), ".yaml"))
}

func processWorkflowFile(ctx context.Context, client *github.Client, org, repoName string, content *github.RepositoryContent, usesRepos map[string]*ActionInfo) {
	fileContent, _, _, err := client.Repositories.GetContents(ctx, org, repoName, content.GetPath(), nil)
	if err != nil {
		log.Printf("Error getting file %s in %s: %v", content.GetPath(), repoName, err)
		return
	}

	decodedContent, err := fileContent.GetContent()
	if err != nil {
		log.Printf("Error decoding content of %s in %s: %v", content.GetPath(), repoName, err)
		return
	}

	extractActionsFromWorkflow(decodedContent, repoName, usesRepos)
}

func extractActionsFromWorkflow(workflowContent, repoName string, usesRepos map[string]*ActionInfo) {
	var workflow map[string]interface{}
	if err := yaml.Unmarshal([]byte(workflowContent), &workflow); err != nil {
		log.Printf("Error unmarshalling YAML in %s: %v", repoName, err)
		return
	}

	jobs, ok := workflow["jobs"].(map[string]interface{})
	if !ok {
		return
	}

	for _, job := range jobs {
		processJobSteps(job, repoName, usesRepos)
	}
}

func processJobSteps(job interface{}, repoName string, usesRepos map[string]*ActionInfo) {
	jobMap, ok := job.(map[string]interface{})
	if !ok {
		return
	}

	steps, ok := jobMap["steps"].([]interface{})
	if !ok {
		return
	}

	for _, step := range steps {
		stepMap, ok := step.(map[string]interface{})
		if !ok {
			continue
		}

		if uses, ok := stepMap["uses"].(string); ok {
			addActionUsage(uses, repoName, usesRepos)
		}
	}
}

func addActionUsage(actionUse, repoName string, usesRepos map[string]*ActionInfo) {
	if usesRepos[actionUse] == nil {
		usesRepos[actionUse] = &ActionInfo{
			Repos:    make(map[string]struct{}),
			Analyzed: false,
		}
	}
	usesRepos[actionUse].Repos[repoName] = struct{}{}
}

func analyzeActions(ctx context.Context, client *github.Client, usesRepos map[string]*ActionInfo) {
	fmt.Println("\nAnalyzing actions...")

	for actionUse, info := range usesRepos {
		if info.Analyzed {
			continue
		}

		owner, repo := parseActionReference(actionUse)
		if owner == "" || repo == "" {
			continue
		}

		fmt.Printf("Analyzing %s/%s...\n", owner, repo)
		analyzeActionDependencies(ctx, client, owner, repo, info)
		info.Analyzed = true
	}
}

func parseActionReference(actionUse string) (string, string) {
	parts := strings.Split(strings.Split(actionUse, "@")[0], "/")
	if len(parts) < 2 {
		return "", "" // Skip invalid action references
	}
	return parts[0], parts[1]
}

func analyzeActionDependencies(ctx context.Context, client *github.Client, owner, repo string, info *ActionInfo) {
	// First try package-lock.json for complete dependency tree
	if analyzePackageLockFile(ctx, client, owner, repo, info) {
		return
	}

	// Fallback to package.json if lock file doesn't exist
	analyzePackageJsonFile(ctx, client, owner, repo, info)
}

func analyzePackageLockFile(ctx context.Context, client *github.Client, owner, repo string, info *ActionInfo) bool {
	packageLock, _, _, err := client.Repositories.GetContents(ctx, owner, repo, "package-lock.json", nil)
	if err != nil {
		return false
	}

	info.UsesNpm = true
	fmt.Printf("  Found package-lock.json for %s/%s\n", owner, repo)

	content, err := packageLock.GetContent()
	if err != nil {
		fmt.Printf("  Error reading package-lock.json content for %s/%s: %v\n", owner, repo, err)
		return true
	}

	var lockJSON map[string]interface{}
	if err := json.Unmarshal([]byte(content), &lockJSON); err != nil {
		fmt.Printf("  Error parsing package-lock.json for %s/%s: %v\n", owner, repo, err)
		return true
	}

	checkPackagesForInfection(lockJSON, info)
	return true
}

func analyzePackageJsonFile(ctx context.Context, client *github.Client, owner, repo string, info *ActionInfo) {
	_, _, _, err := client.Repositories.GetContents(ctx, owner, repo, "package.json", nil)
	if err != nil {
		fmt.Printf("  No package.json or package-lock.json found for %s/%s\n", owner, repo)
		return
	}

	info.UsesNpm = true
	fmt.Printf("  Found package.json (no lock file) for %s/%s\n", owner, repo)
}

func checkPackagesForInfection(lockJSON map[string]interface{}, info *ActionInfo) {
	packages, exists := lockJSON["packages"].(map[string]interface{})
	if !exists {
		return
	}

	foundInfected := []string{}

	for pkgPath, pkgInfo := range packages {
		if pkgPath == "" { // Skip root package
			continue
		}

		pkgData, ok := pkgInfo.(map[string]interface{})
		if !ok {
			continue
		}

		version, ok := pkgData["version"].(string)
		if !ok {
			continue
		}

		pkgName := extractPackageName(pkgPath)
		fullPkg := pkgName + "@" + version

		if isInfectedPackage(fullPkg) {
			foundInfected = append(foundInfected, fullPkg)
		}
	}

	if len(foundInfected) > 0 {
		info.IsInfected = true
		info.InfectedPackages = foundInfected
		fmt.Printf("  ⚠️  INFECTED with %d packages: %v\n", len(foundInfected), foundInfected)
	}
}

func extractPackageName(pkgPath string) string {
	pkgName := strings.TrimPrefix(pkgPath, "node_modules/")

	// Handle scoped packages (e.g., "node_modules/@scope/package" -> "@scope/package")
	if strings.HasPrefix(pkgName, "@") &&
		strings.Contains(pkgName, "/") &&
		!strings.Contains(pkgName, "/node_modules/") {
		return pkgName // Keep scoped package name as is
	}

	// For nested dependencies, take only the last part
	parts := strings.Split(pkgName, "/node_modules/")
	if len(parts) > 1 {
		pkgName = parts[len(parts)-1]
	}

	return pkgName
}

func isInfectedPackage(fullPkg string) bool {
	for _, infectedPkg := range infectedPackages {
		if fullPkg == infectedPkg {
			return true
		}
	}
	return false
}

func printResults(usesRepos map[string]*ActionInfo) {
	fmt.Println("\nActions and the repositories they are used in:")

	for use, info := range usesRepos {
		fmt.Printf("%s:\n", use)
		fmt.Printf("  Uses npm: %t\n", info.UsesNpm)

		if info.IsInfected {
			fmt.Printf("  ⚠️  INFECTED: %t\n", info.IsInfected)
			fmt.Printf("  Infected packages: %v\n", info.InfectedPackages)
		} else {
			fmt.Printf("  Infected: %t\n", info.IsInfected)
		}

		fmt.Printf("  Used in repositories:\n")
		for repo := range info.Repos {
			fmt.Printf("    - %s\n", repo)
		}
		fmt.Println()
	}
}
