package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	defaultForgejoURL     = "https://git.alc.xyz"
	defaultForgejoUser    = "alc"
	defaultGitHubUser     = "alcxyz"
	defaultForgejoSSHHost = "git-ssh.alc.xyz"
	defaultCodebergURL    = "https://codeberg.org"
	defaultCodebergUser   = "alc"
)

type forgejoRepo struct {
	Name           string `json:"name"`
	Mirror         bool   `json:"mirror"`
	CloneURL       string `json:"clone_url"`
	SSHURL         string `json:"ssh_url"`
	OriginalURL    string `json:"original_url"`
	FullName       string `json:"full_name"`
	MirrorInterval string `json:"mirror_interval"`
	DefaultBranch  string `json:"default_branch"`
	Private        bool   `json:"private"`
}

type pushMirror struct {
	RemoteAddress string `json:"remote_address"`
	LastError     string `json:"last_error"`
	LastUpdate    string `json:"last_update"`
	SyncOnCommit  bool   `json:"sync_on_commit"`
}

type forgejoBranchProtection struct {
	RuleName                string   `json:"rule_name"`
	BranchName              string   `json:"branch_name"`
	ApplyToAdmins           bool     `json:"apply_to_admins"`
	EnablePush              bool     `json:"enable_push"`
	EnableMergeWhitelist    bool     `json:"enable_merge_whitelist"`
	MergeWhitelistUsernames []string `json:"merge_whitelist_usernames"`
}

type githubRepo struct {
	DefaultBranch string `json:"default_branch"`
}

type forgejoBranch struct {
	Name   string `json:"name"`
	Commit struct {
		ID string `json:"id"`
	} `json:"commit"`
}

type githubBranch struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	token := os.Getenv("FORGEJO_TOKEN")
	forgejoURL := envOr("FORGEJO_URL", defaultForgejoURL)
	forgejoUser := envOr("FORGEJO_USER", defaultForgejoUser)

	switch os.Args[1] {
	case "sync":
		scanPaths := defaultScanPaths()
		if len(os.Args) > 2 {
			scanPaths = os.Args[2:]
		}
		if err := cmdSync(forgejoURL, forgejoUser, scanPaths); err != nil {
			fmt.Fprintf(os.Stderr, "sync: %v\n", err)
			os.Exit(1)
		}

	case "primary":
		scanPaths := defaultScanPaths()
		if len(os.Args) > 2 {
			scanPaths = os.Args[2:]
		}
		if err := cmdPrimary(forgejoUser, scanPaths); err != nil {
			fmt.Fprintf(os.Stderr, "primary: %v\n", err)
			os.Exit(1)
		}

	case "create":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: forge-mirror create <repo-name>")
			os.Exit(1)
		}
		if token == "" {
			token = readTokenFile()
		}
		if token == "" {
			fmt.Fprintln(os.Stderr, "FORGEJO_TOKEN or FORGEJO_TOKEN_FILE is required for create")
			os.Exit(1)
		}
		if err := cmdCreate(forgejoURL, forgejoUser, token, os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "create: %v\n", err)
			os.Exit(1)
		}

	case "convert":
		if token == "" {
			token = readTokenFile()
		}
		if token == "" {
			fmt.Fprintln(os.Stderr, "FORGEJO_TOKEN or FORGEJO_TOKEN_FILE is required for convert")
			os.Exit(1)
		}
		// Optional: convert specific repos by name, or all mirrors
		var names []string
		if len(os.Args) > 2 {
			names = os.Args[2:]
		}
		if err := cmdConvert(forgejoURL, forgejoUser, token, names); err != nil {
			fmt.Fprintf(os.Stderr, "convert: %v\n", err)
			os.Exit(1)
		}

	case "recreate":
		if token == "" {
			token = readTokenFile()
		}
		if token == "" {
			fmt.Fprintln(os.Stderr, "FORGEJO_TOKEN or FORGEJO_TOKEN_FILE is required for recreate")
			os.Exit(1)
		}
		var names []string
		if len(os.Args) > 2 {
			names = os.Args[2:]
		}
		if len(names) == 0 {
			fmt.Fprintln(os.Stderr, "usage: forge-mirror recreate <repo-name> [repo-name...]")
			fmt.Fprintln(os.Stderr, "       forge-mirror recreate --all")
			os.Exit(1)
		}
		if err := cmdRecreate(forgejoURL, forgejoUser, token, names); err != nil {
			fmt.Fprintf(os.Stderr, "recreate: %v\n", err)
			os.Exit(1)
		}

	case "pull":
		if token == "" {
			token = readTokenFile()
		}
		if token == "" {
			fmt.Fprintln(os.Stderr, "FORGEJO_TOKEN or FORGEJO_TOKEN_FILE is required for pull")
			os.Exit(1)
		}
		var names []string
		if len(os.Args) > 2 {
			names = os.Args[2:]
		}
		if err := cmdPull(forgejoURL, forgejoUser, token, names); err != nil {
			fmt.Fprintf(os.Stderr, "pull: %v\n", err)
			os.Exit(1)
		}

	case "mirror-github":
		if token == "" {
			token = readTokenFile()
		}
		if token == "" {
			fmt.Fprintln(os.Stderr, "FORGEJO_TOKEN or FORGEJO_TOKEN_FILE is required for mirror-github")
			os.Exit(1)
		}
		var names []string
		if len(os.Args) > 2 {
			names = os.Args[2:]
		}
		if err := cmdMirrorGitHub(forgejoURL, forgejoUser, token, names); err != nil {
			fmt.Fprintf(os.Stderr, "mirror-github: %v\n", err)
			os.Exit(1)
		}

	case "mirror-codeberg":
		if token == "" {
			token = readTokenFile()
		}
		if token == "" {
			fmt.Fprintln(os.Stderr, "FORGEJO_TOKEN or FORGEJO_TOKEN_FILE is required for mirror-codeberg")
			os.Exit(1)
		}
		var names []string
		if len(os.Args) > 2 {
			names = os.Args[2:]
		}
		if err := cmdMirrorCodeberg(forgejoURL, forgejoUser, token, names); err != nil {
			fmt.Fprintf(os.Stderr, "mirror-codeberg: %v\n", err)
			os.Exit(1)
		}

	case "status":
		scanPaths := defaultScanPaths()
		if len(os.Args) > 2 {
			scanPaths = os.Args[2:]
		}
		if err := cmdStatus(forgejoURL, forgejoUser, scanPaths); err != nil {
			fmt.Fprintf(os.Stderr, "status: %v\n", err)
			os.Exit(1)
		}

	case "audit":
		if token == "" {
			token = readTokenFile()
		}
		if token == "" {
			fmt.Fprintln(os.Stderr, "FORGEJO_TOKEN or FORGEJO_TOKEN_FILE is required for audit")
			os.Exit(1)
		}
		var names []string
		if len(os.Args) > 2 {
			names = os.Args[2:]
		}
		if err := cmdAudit(forgejoURL, forgejoUser, token, names); err != nil {
			fmt.Fprintf(os.Stderr, "audit: %v\n", err)
			os.Exit(1)
		}

	case "credential-helper":
		// Git credential helper protocol: git calls us with "get" on stdin.
		// We respond with username + password (the Forgejo API token).
		if len(os.Args) >= 3 && os.Args[2] == "get" {
			tok := os.Getenv("FORGEJO_TOKEN")
			if tok == "" {
				tok = readTokenFile()
			}
			if tok != "" {
				fmt.Printf("username=%s\n", forgejoUser)
				fmt.Printf("password=%s\n", tok)
			}
		}

	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `forge-mirror — manage Forgejo-first remotes, GitHub mirrors, and drift audits

Commands:
  sync      [paths...]    Configure legacy dual-push for local repos that have Forgejo repos
  primary   [paths...]    Configure local repos to use Forgejo SSH as origin and GitHub as secondary remote
  audit     [names...]    Audit Forgejo-first drift against GitHub mirrors and branch policy
  create    <repo-name>   Create a new pull mirror on Forgejo for a GitHub repo
  convert   [repo-names]  Convert pull mirrors to regular repos (enables push)
  recreate  <names|--all> Delete and re-create repos as regular (non-mirror) repos
  pull      [names...]    Fetch from GitHub and push to Forgejo (all repos if no names)
  mirror-github [names...] Configure Forgejo push mirrors to GitHub (all repos if no names)
  mirror-codeberg [names...] Configure Forgejo push mirrors to Codeberg (all repos if no names)
  status    [paths...]    Show mirror and push-url status for local repos

Environment:
  FORGEJO_TOKEN        API token (required for create/convert/recreate, takes precedence)
  FORGEJO_TOKEN_FILE   Path to file containing API token (alternative to FORGEJO_TOKEN)
  FORGEJO_URL          Forgejo instance URL (default: https://git.alc.xyz)
  FORGEJO_USER         Forgejo username (default: alc)
  FORGEJO_SSH_HOST     Forgejo SSH hostname for local origin URLs (default: git-ssh.alc.xyz)
  GITHUB_USER          GitHub username (default: alcxyz)
  GITHUB_MIRROR_PAT    GitHub PAT for private repos (falls back to gh auth token)
  CODEBERG_URL         Codeberg base URL (default: https://codeberg.org)
  CODEBERG_USER        Codeberg username (default: alc)
  CODEBERG_MIRROR_PAT  Codeberg PAT for Forgejo push mirrors
  CODEBERG_MIRROR_PAT_FILE Path to file containing Codeberg PAT`)
}

// --- sync command ---

func cmdSync(forgejoURL, forgejoUser string, scanPaths []string) error {
	token := os.Getenv("FORGEJO_TOKEN")
	if token == "" {
		token = readTokenFile()
	}

	mirrors, err := fetchForgejoRepos(forgejoURL, forgejoUser, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot reach Forgejo (%v), skipping sync\n", err)
		return nil // graceful skip when offline
	}

	mirrorNames := make(map[string]string) // repo name → https clone url
	for _, r := range mirrors {
		mirrorNames[r.Name] = r.CloneURL
	}

	repos := findLocalRepos(scanPaths)
	configured := 0
	for _, repoPath := range repos {
		name := inferRepoName(repoPath)
		cloneURL, ok := mirrorNames[name]
		if !ok {
			continue
		}
		changed, err := ensurePushURL(repoPath, cloneURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: error: %v\n", name, err)
			continue
		}
		if changed {
			fmt.Printf("  %s: added Forgejo push URL\n", name)
			configured++
		}
	}

	if configured > 0 {
		fmt.Printf("sync: configured %d repo(s)\n", configured)
	}
	return nil
}

// --- primary command ---

func cmdPrimary(forgejoUser string, scanPaths []string) error {
	token := os.Getenv("FORGEJO_TOKEN")
	if token == "" {
		token = readTokenFile()
	}

	repos := findLocalRepos(scanPaths)
	forgejoRepos := make(map[string]bool)
	remoteRepos, err := fetchForgejoRepos(defaultForgejoURL, forgejoUser, token)
	if err != nil {
		return fmt.Errorf("fetching Forgejo repos: %w", err)
	}
	for _, r := range remoteRepos {
		forgejoRepos[r.Name] = true
	}

	changedCount := 0
	for _, repoPath := range repos {
		name := inferRepoName(repoPath)
		if !forgejoRepos[name] {
			continue
		}

		changed, err := ensureForgejoPrimary(repoPath, forgejoUser, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: error: %v\n", name, err)
			continue
		}
		if changed {
			fmt.Printf("  %s: set Forgejo as origin\n", name)
			changedCount++
		}
	}

	if changedCount > 0 {
		fmt.Printf("primary: configured %d repo(s)\n", changedCount)
	}
	return nil
}

// --- create command ---

func cmdCreate(forgejoURL, forgejoUser, token, repoName string) error {
	githubUser := envOr("GITHUB_USER", defaultGitHubUser)
	cloneAddr := fmt.Sprintf("https://github.com/%s/%s.git", githubUser, repoName)

	// Check if already exists
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/repos/%s/%s", forgejoURL, forgejoUser, repoName))
	if err != nil {
		return fmt.Errorf("checking repo: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		fmt.Printf("%s already exists on Forgejo\n", repoName)
		return nil
	}

	// Create mirror via Forgejo API
	payload := map[string]interface{}{
		"clone_addr":      cloneAddr,
		"repo_name":       repoName,
		"repo_owner":      forgejoUser,
		"mirror":          true,
		"mirror_interval": "8h0m0s",
		"service":         "github",
	}

	// Add GitHub PAT if available for private repos
	if ghToken := os.Getenv("GITHUB_MIRROR_PAT"); ghToken != "" {
		payload["auth_token"] = ghToken
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("%s/api/v1/repos/migrate", forgejoURL),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+token)

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("creating mirror: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	fmt.Printf("created mirror: %s/%s/%s\n", forgejoURL, forgejoUser, repoName)
	return nil
}

// --- convert command ---

func cmdConvert(forgejoURL, forgejoUser, token string, names []string) error {
	githubUser := envOr("GITHUB_USER", defaultGitHubUser)
	ghToken := getGitHubPAT()

	repos, err := fetchForgejoRepos(forgejoURL, forgejoUser, token)
	if err != nil {
		return fmt.Errorf("fetching repos: %w", err)
	}

	// Build filter set (empty = convert all mirrors)
	filter := make(map[string]bool)
	for _, n := range names {
		filter[n] = true
	}

	converted := 0
	for _, r := range repos {
		if !r.Mirror {
			continue
		}
		if len(filter) > 0 && !filter[r.Name] {
			continue
		}

		if err := deleteAndRecreate(forgejoURL, forgejoUser, githubUser, token, ghToken, r.Name); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", r.Name, err)
			continue
		}

		fmt.Printf("  %s: converted to regular repo\n", r.Name)
		converted++
	}

	if converted > 0 {
		fmt.Printf("convert: %d repo(s) converted\n", converted)
	} else if len(filter) == 0 {
		fmt.Println("convert: no mirror repos found")
	} else {
		fmt.Println("convert: no matching mirror repos found")
	}
	return nil
}

// --- recreate command ---

func cmdRecreate(forgejoURL, forgejoUser, token string, names []string) error {
	githubUser := envOr("GITHUB_USER", defaultGitHubUser)
	ghToken := getGitHubPAT()

	// --all: fetch all repos from GitHub via gh CLI
	if len(names) == 1 && names[0] == "--all" {
		out, err := exec.Command("gh", "repo", "list", githubUser,
			"--limit", "200", "--json", "name,isArchived",
			"--jq", ".[] | select(.isArchived == false) | .name").Output()
		if err != nil {
			return fmt.Errorf("listing GitHub repos: %w", err)
		}
		names = nil
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line != "" {
				names = append(names, line)
			}
		}
		fmt.Printf("recreate: found %d non-archived GitHub repos\n", len(names))
	}

	// Fetch existing Forgejo repos (authenticated to see private repos)
	existing := make(map[string]bool)
	repos, err := fetchForgejoRepos(forgejoURL, forgejoUser, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot list Forgejo repos: %v\n", err)
	}
	for _, r := range repos {
		existing[r.Name] = true
	}

	succeeded := 0
	failed := 0
	for _, name := range names {
		action := "creating"
		if existing[name] {
			action = "recreating"
		}
		fmt.Printf("  %s: %s...\n", name, action)

		if existing[name] {
			if err := deleteForgejoRepo(forgejoURL, forgejoUser, token, name); err != nil {
				fmt.Fprintf(os.Stderr, "  %s: delete failed: %v\n", name, err)
				failed++
				continue
			}
		}

		if err := migrateAsRegular(forgejoURL, forgejoUser, githubUser, token, ghToken, name); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: migrate failed: %v\n", name, err)
			failed++
			continue
		}

		fmt.Printf("  %s: done\n", name)
		succeeded++
	}

	fmt.Printf("recreate: %d succeeded, %d failed\n", succeeded, failed)
	if failed > 0 {
		return fmt.Errorf("%d repo(s) failed", failed)
	}
	return nil
}

// --- shared helpers for delete + migrate ---

func deleteForgejoRepo(forgejoURL, forgejoUser, token, name string) error {
	req, _ := http.NewRequest("DELETE",
		fmt.Sprintf("%s/api/v1/repos/%s/%s", forgejoURL, forgejoUser, name),
		nil)
	req.Header.Set("Authorization", "token "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP error: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func migrateAsRegular(forgejoURL, forgejoUser, githubUser, token, ghToken, name string) error {
	cloneAddr := fmt.Sprintf("https://github.com/%s/%s.git", githubUser, name)
	payload := map[string]interface{}{
		"clone_addr": cloneAddr,
		"repo_name":  name,
		"repo_owner": forgejoUser,
		"mirror":     false,
		"service":    "github",
	}
	if ghToken != "" {
		payload["auth_token"] = ghToken
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("%s/api/v1/repos/migrate", forgejoURL),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func deleteAndRecreate(forgejoURL, forgejoUser, githubUser, token, ghToken, name string) error {
	fmt.Printf("  %s: deleting...\n", name)
	if err := deleteForgejoRepo(forgejoURL, forgejoUser, token, name); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if err := migrateAsRegular(forgejoURL, forgejoUser, githubUser, token, ghToken, name); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// getGitHubPAT returns a GitHub token for cloning private repos.
// Checks GITHUB_MIRROR_PAT env var first, then falls back to gh auth token.
func getGitHubPAT() string {
	if pat := os.Getenv("GITHUB_MIRROR_PAT"); pat != "" {
		return pat
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func getCodebergPAT() string {
	return readSecretEnv("CODEBERG_MIRROR_PAT", "CODEBERG_MIRROR_PAT_FILE")
}

// --- mirror-github command ---

func cmdMirrorGitHub(forgejoURL, forgejoUser, token string, names []string) error {
	githubUser := envOr("GITHUB_USER", defaultGitHubUser)
	ghToken := getGitHubPAT()
	if ghToken == "" {
		return fmt.Errorf("GITHUB_MIRROR_PAT or gh auth token is required")
	}

	repos, err := fetchForgejoRepos(forgejoURL, forgejoUser, token)
	if err != nil {
		return fmt.Errorf("fetching repos: %w", err)
	}

	return cmdMirrorRemote(
		forgejoURL,
		forgejoUser,
		token,
		repos,
		names,
		"mirror-github",
		"GitHub",
		githubMirrorTarget(githubUser),
		githubUser,
		ghToken,
	)
}

func cmdMirrorCodeberg(forgejoURL, forgejoUser, token string, names []string) error {
	codebergUser := envOr("CODEBERG_USER", defaultCodebergUser)
	codebergToken := getCodebergPAT()
	if codebergToken == "" {
		return fmt.Errorf("CODEBERG_MIRROR_PAT or CODEBERG_MIRROR_PAT_FILE is required")
	}

	repos, err := fetchForgejoRepos(forgejoURL, forgejoUser, token)
	if err != nil {
		return fmt.Errorf("fetching repos: %w", err)
	}

	return cmdMirrorRemote(
		forgejoURL,
		forgejoUser,
		token,
		repos,
		names,
		"mirror-codeberg",
		"Codeberg",
		codebergMirrorTarget(envOr("CODEBERG_URL", defaultCodebergURL), codebergUser),
		codebergUser,
		codebergToken,
	)
}

func cmdMirrorRemote(forgejoURL, forgejoUser, token string, repos []forgejoRepo, names []string, commandName, label string, targetForRepo func(string) string, remoteUsername, remotePassword string) error {
	if remotePassword == "" {
		return fmt.Errorf("%s credentials are required", label)
	}

	filter := make(map[string]bool)
	for _, n := range names {
		filter[n] = true
	}

	created := 0
	skipped := 0
	for _, r := range repos {
		if len(filter) > 0 && !filter[r.Name] {
			continue
		}

		target := targetForRepo(r.Name)
		exists, err := hasPushMirror(forgejoURL, forgejoUser, token, r.Name, target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: list mirrors failed: %v\n", r.Name, err)
			continue
		}
		if exists {
			skipped++
			fmt.Printf("  %s: %s push mirror already exists\n", r.Name, label)
			continue
		}

		if err := createPushMirror(forgejoURL, forgejoUser, token, r.Name, target, remoteUsername, remotePassword); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: create push mirror failed: %v\n", r.Name, err)
			continue
		}
		fmt.Printf("  %s: %s push mirror created\n", r.Name, label)
		created++
	}

	fmt.Printf("%s: %d created, %d already present\n", commandName, created, skipped)
	return nil
}

// --- pull command ---

func cmdPull(forgejoURL, forgejoUser, token string, names []string) error {
	ghToken := getGitHubPAT()

	repos, err := fetchForgejoRepos(forgejoURL, forgejoUser, token)
	if err != nil {
		return fmt.Errorf("fetching repos: %w", err)
	}

	// Build filter set (empty = pull all)
	filter := make(map[string]bool)
	for _, n := range names {
		filter[n] = true
	}

	// Build authenticated Forgejo push URL
	forgejoAuthURL := forgejoURL
	if token != "" {
		for _, scheme := range []string{"https://", "http://"} {
			if strings.HasPrefix(forgejoURL, scheme) {
				forgejoAuthURL = strings.Replace(forgejoURL, scheme, fmt.Sprintf("%s%s:%s@", scheme, forgejoUser, token), 1)
				break
			}
		}
	}

	tmpBase, err := os.MkdirTemp("", "forge-mirror-pull-")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpBase)

	pulled := 0
	failed := 0
	for _, r := range repos {
		if r.OriginalURL == "" || !strings.Contains(r.OriginalURL, "github.com") {
			continue
		}
		if len(filter) > 0 && !filter[r.Name] {
			continue
		}

		// GitHub clone URL with auth (fine-grained PATs use x-access-token as username)
		cloneURL := r.OriginalURL
		if ghToken != "" {
			cloneURL = strings.Replace(cloneURL, "https://github.com/", fmt.Sprintf("https://x-access-token:%s@github.com/", ghToken), 1)
		}

		// Forgejo push URL with auth
		pushURL := fmt.Sprintf("%s/%s/%s.git", forgejoAuthURL, forgejoUser, r.Name)

		tmpDir := filepath.Join(tmpBase, r.Name)

		// Bare clone from GitHub
		cmd := exec.Command("git", "clone", "--bare", "--quiet", cloneURL, tmpDir)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: clone failed: %v\n", r.Name, err)
			failed++
			continue
		}

		// Push to Forgejo (non-force: only fast-forwards)
		pushCmd := exec.Command("git", "-C", tmpDir, "push", "--quiet", pushURL, "--all")
		pushCmd.Stderr = os.Stderr
		pushErr := pushCmd.Run()

		tagCmd := exec.Command("git", "-C", tmpDir, "push", "--quiet", pushURL, "--tags")
		tagCmd.Stderr = os.Stderr
		tagErr := tagCmd.Run()

		if pushErr != nil || tagErr != nil {
			fmt.Fprintf(os.Stderr, "  %s: push failed (branches: %v, tags: %v)\n", r.Name, pushErr, tagErr)
			failed++
			continue
		}

		fmt.Printf("  %s: synced\n", r.Name)
		pulled++

		// Clean up each repo as we go to save disk space
		os.RemoveAll(tmpDir)
	}

	if pulled > 0 || failed > 0 {
		fmt.Printf("pull: %d synced, %d failed\n", pulled, failed)
	} else {
		fmt.Println("pull: nothing to sync")
	}
	// Best-effort: don't fail the service for partial sync failures
	return nil
}

// --- status command ---

func cmdStatus(forgejoURL, forgejoUser string, scanPaths []string) error {
	token := os.Getenv("FORGEJO_TOKEN")
	if token == "" {
		token = readTokenFile()
	}

	remoteRepos, err := fetchForgejoRepos(forgejoURL, forgejoUser, token)
	if err != nil {
		return fmt.Errorf("fetching repos: %w", err)
	}

	type repoInfo struct {
		cloneURL string
		mirror   bool
	}
	repoMap := make(map[string]repoInfo)
	for _, r := range remoteRepos {
		repoMap[r.Name] = repoInfo{cloneURL: r.CloneURL, mirror: r.Mirror}
	}

	repos := findLocalRepos(scanPaths)
	for _, repoPath := range repos {
		name := inferRepoName(repoPath)
		info, found := repoMap[name]
		if !found {
			continue
		}

		pushURLs := getExplicitPushURLs(repoPath)
		hasForgejo := false
		for _, u := range pushURLs {
			if strings.Contains(u, "git.alc.xyz") {
				hasForgejo = true
				break
			}
		}

		repoType := "regular"
		if info.mirror {
			repoType = "mirror "
		}

		pushStatus := "no push-url"
		if hasForgejo && info.mirror {
			pushStatus = "push-blocked"
		} else if hasForgejo {
			pushStatus = "dual-push"
		}
		fmt.Printf("  %-30s  %s  %-13s  %s\n", name, repoType, pushStatus, info.cloneURL)
	}

	return nil
}

// --- audit command ---

func cmdAudit(forgejoURL, forgejoUser, token string, names []string) error {
	githubUser := envOr("GITHUB_USER", defaultGitHubUser)
	ghToken := getGitHubPAT()

	repos, err := fetchForgejoRepos(forgejoURL, forgejoUser, token)
	if err != nil {
		return fmt.Errorf("fetching repos: %w", err)
	}

	filter := make(map[string]bool)
	for _, n := range names {
		filter[n] = true
	}

	driftCount := 0
	checked := 0
	for _, repo := range repos {
		if len(filter) > 0 && !filter[repo.Name] {
			continue
		}
		target := fmt.Sprintf("https://github.com/%s/%s.git", githubUser, repo.Name)
		if repo.OriginalURL != "" && strings.Contains(repo.OriginalURL, "github.com") {
			target = repo.OriginalURL
		}

		result, err := auditRepo(forgejoURL, forgejoUser, token, ghToken, githubUser, repo, target)
		if err != nil {
			fmt.Printf("  %s: audit error: %v\n", repo.Name, err)
			driftCount++
			checked++
			continue
		}
		checked++

		if len(result.issues) == 0 {
			fmt.Printf("  %s: ok\n", repo.Name)
			continue
		}

		fmt.Printf("  %s:\n", repo.Name)
		for _, issue := range result.issues {
			fmt.Printf("    - %s\n", issue)
		}
		driftCount++
	}

	if checked == 0 {
		fmt.Println("audit: no matching repos")
		return nil
	}
	if driftCount == 0 {
		fmt.Printf("audit: %d repo(s) checked, no drift\n", checked)
		return nil
	}
	return fmt.Errorf("%d repo(s) with drift or policy violations", driftCount)
}

type auditResult struct {
	issues []string
}

func auditRepo(forgejoURL, forgejoUser, forgejoToken, ghToken, githubUser string, repo forgejoRepo, githubCloneURL string) (auditResult, error) {
	var result auditResult

	ghMeta, err := fetchGitHubRepo(githubUser, repo.Name, ghToken)
	if err != nil {
		return result, fmt.Errorf("fetching GitHub repo: %w", err)
	}
	if repo.DefaultBranch != "dev" {
		result.issues = append(result.issues, fmt.Sprintf("Forgejo default branch is %q, expected %q", repo.DefaultBranch, "dev"))
	}
	if ghMeta.DefaultBranch != "dev" {
		result.issues = append(result.issues, fmt.Sprintf("GitHub default branch is %q, expected %q", ghMeta.DefaultBranch, "dev"))
	}

	protection, err := fetchForgejoMainProtection(forgejoURL, forgejoUser, forgejoToken, repo.Name)
	if err != nil {
		result.issues = append(result.issues, fmt.Sprintf("cannot read Forgejo branch protections: %v", err))
	} else if protection == nil {
		result.issues = append(result.issues, "Forgejo main branch protection missing")
	} else {
		if !protection.ApplyToAdmins {
			result.issues = append(result.issues, "Forgejo main protection does not apply to admins")
		}
		if protection.EnablePush {
			result.issues = append(result.issues, "Forgejo main protection still allows direct push")
		}
		if !protection.EnableMergeWhitelist || !containsString(protection.MergeWhitelistUsernames, forgejoUser) {
			result.issues = append(result.issues, fmt.Sprintf("Forgejo main merge whitelist does not include %q", forgejoUser))
		}
	}

	mirror, err := fetchGitHubPushMirror(forgejoURL, forgejoUser, forgejoToken, repo.Name, fmt.Sprintf("https://github.com/%s/%s.git", githubUser, repo.Name))
	if err != nil {
		result.issues = append(result.issues, fmt.Sprintf("cannot read Forgejo push mirrors: %v", err))
	} else if mirror == nil {
		result.issues = append(result.issues, "Forgejo GitHub push mirror missing")
	} else {
		if !mirror.SyncOnCommit {
			result.issues = append(result.issues, "Forgejo GitHub push mirror does not sync on commit")
		}
		if strings.TrimSpace(mirror.LastUpdate) == "" {
			result.issues = append(result.issues, "Forgejo GitHub push mirror has never completed a sync")
		}
	}

	forgejoHeads, err := fetchForgejoBranchHeads(forgejoURL, forgejoUser, forgejoToken, repo.Name)
	if err != nil {
		return result, fmt.Errorf("listing Forgejo heads: %w", err)
	}
	githubHeads, err := fetchGitHubBranchHeads(githubUser, repo.Name, ghToken)
	if err != nil {
		return result, fmt.Errorf("listing GitHub heads: %w", err)
	}

	allBranches := unionKeys(forgejoHeads, githubHeads)
	for _, branch := range allBranches {
		fgSHA, fgOK := forgejoHeads[branch]
		ghSHA, ghOK := githubHeads[branch]
		switch {
		case fgOK && !ghOK:
			result.issues = append(result.issues, fmt.Sprintf("branch %q exists only on Forgejo", branch))
		case !fgOK && ghOK:
			result.issues = append(result.issues, fmt.Sprintf("branch %q exists only on GitHub", branch))
		case fgSHA != ghSHA:
			result.issues = append(result.issues, fmt.Sprintf("branch %q differs (Forgejo %s, GitHub %s)", branch, shortSHA(fgSHA), shortSHA(ghSHA)))
		}
	}

	return result, nil
}

// --- helpers ---

func fetchForgejoRepos(forgejoURL, user string, authTokens ...string) ([]forgejoRepo, error) {
	var all []forgejoRepo
	page := 1
	for {
		url := fmt.Sprintf("%s/api/v1/repos/search?owner=%s&limit=50&page=%d", forgejoURL, user, page)
		req, _ := http.NewRequest("GET", url, nil)
		if len(authTokens) > 0 && authTokens[0] != "" {
			req.Header.Set("Authorization", "token "+authTokens[0])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		var result struct {
			Data []forgejoRepo `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, err
		}
		if len(result.Data) == 0 {
			break
		}
		all = append(all, result.Data...)
		page++
	}
	return all, nil
}

func findLocalRepos(scanPaths []string) []string {
	var repos []string
	for _, base := range scanPaths {
		base = expandHome(base)
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			candidate := filepath.Join(base, e.Name())
			if isGitRepo(candidate) {
				repos = append(repos, candidate)
			}
		}
	}
	return repos
}

func isGitRepo(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil // .git can be a directory (regular) or file (submodule/worktree)
}

func ensurePushURL(repoPath, forgejoHTTPS string) (bool, error) {
	// Check if pushurl is explicitly configured in git config (not just
	// the implicit fallback from the fetch url).
	explicitPushURLs := getExplicitPushURLs(repoPath)

	// Migrate: remove any old SSH push URLs for Forgejo
	migrated := false
	for _, u := range explicitPushURLs {
		if strings.Contains(u, "git.alc.xyz") && strings.HasPrefix(u, "ssh://") {
			gitCmd(repoPath, "remote", "set-url", "--delete", "--push", "origin", u)
			migrated = true
		}
	}
	if migrated {
		// Re-read after cleanup
		explicitPushURLs = getExplicitPushURLs(repoPath)
		// If we removed all push URLs (only had SSH), reset to clean state
		if len(explicitPushURLs) == 0 {
			// No explicit URLs left — git falls back to fetch URL, which is fine
		}
	}

	// Already has the correct HTTPS push URL
	for _, u := range explicitPushURLs {
		if u == forgejoHTTPS {
			return migrated, nil
		}
	}

	// Get the current origin URL (fetch URL)
	originURL := getGitOriginURL(repoPath)
	if originURL == "" {
		return false, fmt.Errorf("no origin remote")
	}

	// If no explicit pushurl is set, git uses the fetch url implicitly.
	// We must add the fetch url as an explicit pushurl first so it isn't
	// lost when we add Forgejo.
	if len(explicitPushURLs) == 0 {
		if err := gitCmd(repoPath, "remote", "set-url", "--add", "--push", "origin", originURL); err != nil {
			return false, err
		}
	}

	// Add Forgejo HTTPS push URL
	if err := gitCmd(repoPath, "remote", "set-url", "--add", "--push", "origin", forgejoHTTPS); err != nil {
		return false, err
	}

	return true, nil
}

// getExplicitPushURLs reads pushurl entries directly from git config,
// avoiding the implicit fallback that git-remote-get-url --push uses.
func getExplicitPushURLs(repoPath string) []string {
	out, err := exec.Command("git", "-C", repoPath, "config", "--get-all", "remote.origin.pushurl").Output()
	if err != nil {
		return nil // no explicit pushurl set
	}
	var urls []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			urls = append(urls, line)
		}
	}
	return urls
}

func getGitOriginURL(repoPath string) string {
	out, err := exec.Command("git", "-C", repoPath, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func inferRepoName(repoPath string) string {
	originURL := getGitOriginURL(repoPath)
	if originURL == "" {
		return filepath.Base(repoPath)
	}

	trimmed := strings.TrimSuffix(originURL, ".git")
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 && idx+1 < len(trimmed) {
		return trimmed[idx+1:]
	}
	if idx := strings.LastIndex(trimmed, ":"); idx >= 0 && idx+1 < len(trimmed) {
		return trimmed[idx+1:]
	}
	return filepath.Base(repoPath)
}

func getGitRemoteURL(repoPath, remoteName string) string {
	out, err := exec.Command("git", "-C", repoPath, "remote", "get-url", remoteName).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func ensureForgejoPrimary(repoPath, forgejoUser, repoName string) (bool, error) {
	sshHost := envOr("FORGEJO_SSH_HOST", defaultForgejoSSHHost)
	forgejoSSH := fmt.Sprintf("git@%s:%s/%s.git", sshHost, forgejoUser, repoName)
	originURL := getGitOriginURL(repoPath)
	if originURL == "" {
		return false, fmt.Errorf("no origin remote")
	}

	changed := false

	if strings.Contains(originURL, "github.com") {
		if err := ensureRemoteURL(repoPath, "github", originURL); err != nil {
			return false, err
		}
	}

	if originURL != forgejoSSH {
		if err := gitCmd(repoPath, "remote", "set-url", "origin", forgejoSSH); err != nil {
			return false, err
		}
		changed = true
	}

	if err := clearOriginPushURLs(repoPath); err != nil {
		return false, err
	}

	return changed, nil
}

func ensureRemoteURL(repoPath, remoteName, url string) error {
	current := getGitRemoteURL(repoPath, remoteName)
	if current == url {
		return nil
	}
	if current == "" {
		return gitCmd(repoPath, "remote", "add", remoteName, url)
	}
	return gitCmd(repoPath, "remote", "set-url", remoteName, url)
}

func clearOriginPushURLs(repoPath string) error {
	cmd := exec.Command("git", "-C", repoPath, "config", "--unset-all", "remote.origin.pushurl")
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 5 {
			return nil
		}
		return err
	}
	return nil
}

func hasPushMirror(forgejoURL, forgejoUser, token, repoName, remoteAddress string) (bool, error) {
	mirror, err := fetchGitHubPushMirror(forgejoURL, forgejoUser, token, repoName, remoteAddress)
	return mirror != nil, err
}

func fetchGitHubPushMirror(forgejoURL, forgejoUser, token, repoName, remoteAddress string) (*pushMirror, error) {
	req, _ := http.NewRequest("GET",
		fmt.Sprintf("%s/api/v1/repos/%s/%s/push_mirrors", forgejoURL, forgejoUser, repoName),
		nil)
	req.Header.Set("Authorization", "token "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var mirrors []pushMirror
	if err := json.NewDecoder(resp.Body).Decode(&mirrors); err != nil {
		return nil, err
	}
	for _, m := range mirrors {
		if m.RemoteAddress == remoteAddress {
			return &m, nil
		}
	}
	return nil, nil
}

func createPushMirror(forgejoURL, forgejoUser, token, repoName, remoteAddress, remoteUsername, remotePassword string) error {
	payload := map[string]interface{}{
		"remote_address":  remoteAddress,
		"remote_username": remoteUsername,
		"remote_password": remotePassword,
		"interval":        "8h",
		"sync_on_commit":  true,
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("%s/api/v1/repos/%s/%s/push_mirrors", forgejoURL, forgejoUser, repoName),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func gitCmd(repoPath string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", repoPath}, args...)...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func defaultScanPaths() []string {
	home := os.Getenv("HOME")
	return []string{
		filepath.Join(home, "src", "infra"),
		filepath.Join(home, "src", "apps"),
		filepath.Join(home, "src", "tools"),
		filepath.Join(home, "src", "forks"),
		filepath.Join(home, "src", "personal"),
		filepath.Join(home, "src", "sites"),
	}
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home := os.Getenv("HOME")
		return filepath.Join(home, path[2:])
	}
	return path
}

func readTokenFile() string {
	return readSecretEnv("FORGEJO_TOKEN", "FORGEJO_TOKEN_FILE")
}

func readSecretEnv(valueKey, fileKey string) string {
	if value := os.Getenv(valueKey); value != "" {
		return strings.TrimSpace(value)
	}
	path := os.Getenv(fileKey)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func githubMirrorTarget(githubUser string) func(string) string {
	return func(repoName string) string {
		return fmt.Sprintf("https://github.com/%s/%s.git", githubUser, repoName)
	}
}

func codebergMirrorTarget(codebergURL, codebergUser string) func(string) string {
	base := strings.TrimSuffix(codebergURL, "/")
	return func(repoName string) string {
		return fmt.Sprintf("%s/%s/%s.git", base, codebergUser, repoName)
	}
}

func fetchGitHubRepo(owner, repo, token string) (githubRepo, error) {
	var out githubRepo
	req, _ := http.NewRequest("GET", fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo), nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return out, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

func fetchForgejoMainProtection(forgejoURL, forgejoUser, token, repoName string) (*forgejoBranchProtection, error) {
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/repos/%s/%s/branch_protections", forgejoURL, forgejoUser, repoName), nil)
	req.Header.Set("Authorization", "token "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var protections []forgejoBranchProtection
	if err := json.NewDecoder(resp.Body).Decode(&protections); err != nil {
		return nil, err
	}
	for _, protection := range protections {
		if protection.RuleName == "main" || protection.BranchName == "main" {
			return &protection, nil
		}
	}
	return nil, nil
}

func fetchForgejoBranchHeads(forgejoURL, forgejoUser, token, repoName string) (map[string]string, error) {
	heads := make(map[string]string)
	page := 1
	for {
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/repos/%s/%s/branches?page=%d&limit=50", forgejoURL, forgejoUser, repoName, page), nil)
		req.Header.Set("Authorization", "token "+token)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var branches []forgejoBranch
		if err := json.NewDecoder(resp.Body).Decode(&branches); err != nil {
			return nil, err
		}
		if len(branches) == 0 {
			break
		}
		for _, branch := range branches {
			heads[branch.Name] = branch.Commit.ID
		}
		page++
	}
	return heads, nil
}

func fetchGitHubBranchHeads(owner, repo, token string) (map[string]string, error) {
	heads := make(map[string]string)
	page := 1
	for {
		req, _ := http.NewRequest("GET", fmt.Sprintf("https://api.github.com/repos/%s/%s/branches?per_page=100&page=%d", owner, repo, page), nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var branches []githubBranch
		if err := json.NewDecoder(resp.Body).Decode(&branches); err != nil {
			return nil, err
		}
		if len(branches) == 0 {
			break
		}
		for _, branch := range branches {
			heads[branch.Name] = branch.Commit.SHA
		}
		page++
	}
	return heads, nil
}

func unionKeys(a, b map[string]string) []string {
	seen := make(map[string]bool)
	var out []string
	for k := range a {
		seen[k] = true
		out = append(out, k)
	}
	for k := range b {
		if !seen[k] {
			out = append(out, k)
		}
	}
	// Small sets; stable order is enough.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
