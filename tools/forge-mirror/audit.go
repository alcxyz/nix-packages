package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// Audit policy is supplied by the operator, never compiled into the package.
type auditPolicy struct {
	SchemaVersion int               `json:"schema_version"`
	Repositories  []auditRepository `json:"repositories"`
}
type auditRepository struct {
	GitHubPrimary    bool                              `json:"-"`
	DenyGitHubSource bool                              `json:"-"`
	FullName         string                            `json:"full_name"`
	Class            string                            `json:"class"`
	Canonical        string                            `json:"canonical"`
	GitHub           string                            `json:"github"`
	Mirror           string                            `json:"mirror"`
	DefaultBranch    string                            `json:"default_branch"`
	RequiredPrivate  bool                              `json:"required_private"`
	Retired          bool                              `json:"retired"`
	Protections      map[string]map[string]interface{} `json:"protections"`
}
type auditFailure struct{ Findings, Errors int }

func (e *auditFailure) Error() string {
	return fmt.Sprintf("%d policy finding(s), %d execution error(s)", e.Findings, e.Errors)
}
func auditExitCode(err error) int {
	if err == nil {
		return 0
	}
	var failure *auditFailure
	if errors.As(err, &failure) && failure.Errors == 0 {
		return 1
	}
	return 2
}
func loadAuditPolicy() (auditPolicy, error) {
	var policy auditPolicy
	path := os.Getenv("FORGE_MIRROR_AUDIT_POLICY_FILE")
	if path == "" {
		return policy, fmt.Errorf("FORGE_MIRROR_AUDIT_POLICY_FILE is required; supply an explicit repository audit policy")
	}
	file, err := os.Open(path)
	if err != nil {
		return policy, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return policy, err
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		return policy, fmt.Errorf("audit policy must contain one JSON object")
	}
	if policy.SchemaVersion != 1 || len(policy.Repositories) == 0 {
		return policy, fmt.Errorf("audit policy requires schema_version 1 and repositories")
	}
	seen := map[string]bool{}
	for _, repo := range policy.Repositories {
		if !validAuditName(repo.FullName) || seen[strings.ToLower(repo.FullName)] {
			return policy, fmt.Errorf("invalid or duplicate repository in audit policy")
		}
		seen[strings.ToLower(repo.FullName)] = true
		if repo.Retired && len(repo.Protections) > 0 {
			return policy, fmt.Errorf("retired policy cannot declare active protection contracts")
		}
		if repo.GitHub != "" && !validAuditName(repo.GitHub) {
			return policy, fmt.Errorf("invalid GitHub repository in audit policy")
		}
		if !containsString([]string{"forgejo", "github", "upstream", "review", "archived"}, repo.Canonical) || !containsString([]string{"optional", "required", "forbidden", "disabled", "review", "continuity"}, repo.Mirror) {
			return policy, fmt.Errorf("invalid canonical host or mirror policy for %s", repo.FullName)
		}
	}
	return policy, nil
}
func validAuditName(name string) bool {
	owner, repo, ok := strings.Cut(name, "/")
	return ok && auditNamePart(owner) && auditNamePart(repo)
}
func auditNamePart(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
			return false
		}
	}
	return true
}

func cmdAudit(forgejoURL, forgejoUser, githubUser, token string, names []string) error {
	policy, err := loadAuditPolicy()
	if err != nil {
		return err
	}
	primary, err := configuredGithubPrimaryRepos()
	if err != nil {
		return err
	}
	// Keep independently activated direction and publication denials additive.
	denied, err := configuredGithubDeniedRepos()
	if err != nil {
		return err
	}
	private, err := configuredRequiredPrivateRepos()
	if err != nil {
		return err
	}
	repos, err := fetchAccessibleForgejoRepos(forgejoURL, token)
	if err != nil {
		return err
	}
	live := map[string]forgejoRepo{}
	declared := map[string]bool{}
	for _, repo := range repos {
		if !validAuditName(repo.FullName) {
			return fmt.Errorf("Forgejo search returned a repository without a valid full_name")
		}
		live[strings.ToLower(repo.FullName)] = repo
	}
	for _, repo := range policy.Repositories {
		declared[strings.ToLower(repo.FullName)] = true
	}
	// Discoveries outside inventory remain visible as unresolved policy, with
	// best-effort branch checks. No unknown target is silently made optional.
	for key, repo := range live {
		if !declared[key] {
			policy.Repositories = append(policy.Repositories, auditRepository{FullName: repo.FullName, Class: "unclassified", Canonical: "review", Mirror: "review", GitHub: githubUser + "/" + repo.Name})
		}
	}
	sort.Slice(policy.Repositories, func(i, j int) bool { return policy.Repositories[i].FullName < policy.Repositories[j].FullName })
	failure := &auditFailure{}
	checked, retired, flagged := 0, 0, 0
	ghToken := getGitHubPAT()
	for _, rule := range policy.Repositories {
		owner, name, _ := strings.Cut(rule.FullName, "/")
		if len(names) > 0 && !containsString(names, rule.FullName) && !containsString(names, name) {
			continue
		}
		rule.GitHubPrimary = primary[strings.ToLower(name)] || primary[strings.ToLower(rule.FullName)]
		isDenied := denied[strings.ToLower(name)] || denied[strings.ToLower(rule.FullName)]
		rule.RequiredPrivate = rule.RequiredPrivate || private[strings.ToLower(name)] || private[strings.ToLower(rule.FullName)]
		if isDenied {
			rule.Mirror = "forbidden"
			rule.DenyGitHubSource = true
		}
		repo, ok := live[strings.ToLower(rule.FullName)]
		if rule.Retired && !rule.RequiredPrivate && rule.Mirror != "forbidden" && !rule.GitHubPrimary {
			retired++
			fmt.Printf("  %s [%s]: excluded (retired policy)\n", rule.FullName, rule.Class)
			continue
		}
		var findings, execution []string
		if !ok {
			// Search is permission-filtered. Read the declared path so absence/access
			// failures cannot disappear from the report.
			err := auditGet(forgejoURL+"/api/v1/repos/"+owner+"/"+name, token, false, &repo)
			if err != nil {
				execution = append(execution, "reading declared Forgejo repository: "+err.Error())
				if rule.Canonical == "review" || rule.Mirror == "review" {
					findings = append(findings, "repository policy requires owner review")
				}
			}
		}
		if len(execution) == 0 {
			findings, execution = auditDeclaredRepo(forgejoURL, token, ghToken, repo, rule)
		}
		checked++
		fmt.Printf("  %s [%s]:", rule.FullName, rule.Class)
		if len(findings) == 0 && len(execution) == 0 {
			fmt.Println(" ok")
		} else {
			fmt.Println()
			flagged++
		}
		for _, finding := range findings {
			fmt.Printf("    finding: %s\n", finding)
			failure.Findings++
		}
		for _, e := range execution {
			fmt.Printf("    execution error: %s\n", e)
			failure.Errors++
		}
	}
	fmt.Printf("audit: %d repositories checked, %d excluded by retired policy, %d flagged, %d policy findings, %d execution errors\n", checked, retired, flagged, failure.Findings, failure.Errors)
	if failure.Findings > 0 || failure.Errors > 0 {
		return failure
	}
	return nil
}

// API error bodies can contain credentials (for example mirror URLs), so the
// audit reports only the status, never the body or a server-provided URL.
type auditHTTPError int

func (e auditHTTPError) Error() string {
	if int(e) == 404 {
		return "HTTP 404 (absent or inaccessible)"
	}
	return fmt.Sprintf("HTTP %d", int(e))
}
func auditGet(address, token string, github bool, out interface{}) error {
	req, err := http.NewRequest("GET", address, nil)
	if err != nil {
		return fmt.Errorf("invalid API endpoint")
	}
	if token != "" {
		scheme := "token "
		if github {
			scheme = "Bearer "
		}
		req.Header.Set("Authorization", scheme+token)
	}
	req.Header.Set("Accept", "application/json")
	client := *http.DefaultClient
	client.Timeout = 30 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("API request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return auditHTTPError(resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("invalid API response")
	}
	return nil
}
func auditDeclaredRepo(base, token, ghToken string, repo forgejoRepo, rule auditRepository) ([]string, []string) {
	var findings, execution []string
	if rule.RequiredPrivate && !repo.Private {
		findings = append(findings, "Forgejo repository is public but policy requires private visibility")
	}
	if rule.Canonical == "review" || rule.Mirror == "review" {
		findings = append(findings, "repository policy requires owner review")
	}
	if rule.DenyGitHubSource && repo.Mirror && isGitHubRemote(repo.OriginalURL) {
		findings = append(findings, "GitHub pull mirror source denied by activated publication policy")
	}
	if rule.Canonical == "forgejo" && repo.Mirror {
		findings = append(findings, "Forgejo-canonical repository is a pull mirror")
	}
	root := base + "/api/v1/repos/" + rule.FullName
	// Always enumerate outgoing mirrors, including forbidden and retired private
	// history. A healthy sync never excuses publication or direction violations.
	mirrors := []pushMirror{}
	mirrorOK := true
	for page := 1; ; page++ {
		var batch []pushMirror
		if err := auditGet(fmt.Sprintf("%s/push_mirrors?limit=50&page=%d", root, page), token, false, &batch); err != nil {
			execution = append(execution, "listing push mirrors: "+err.Error())
			mirrorOK = false
			break
		}
		if len(batch) == 0 {
			break
		}
		mirrors = append(mirrors, batch...)
	}
	targetPresent := false
	for _, mirror := range mirrors {
		gh := isGitHubRemote(mirror.RemoteAddress)
		switch {
		case rule.Mirror == "forbidden":
			findings = append(findings, "outgoing push mirror forbidden by repository policy")
		case rule.Mirror == "disabled":
			findings = append(findings, "outgoing push mirror present for retired repository")
		case (rule.Canonical == "github" || rule.GitHubPrimary) && gh:
			findings = append(findings, "Forgejo GitHub push mirror present for GitHub-primary repository")
		case !gh:
			findings = append(findings, "outgoing push mirror targets a forbidden host")
		case gh && !strings.EqualFold(remoteRepoPath(mirror.RemoteAddress), rule.GitHub):
			findings = append(findings, "outgoing GitHub push mirror targets an undeclared destination")
		}
		if gh && strings.EqualFold(remoteRepoPath(mirror.RemoteAddress), rule.GitHub) {
			targetPresent = true
			if !mirror.SyncOnCommit {
				findings = append(findings, "GitHub push mirror does not sync on commit")
			}
			if strings.TrimSpace(mirror.LastUpdate) == "" {
				findings = append(findings, "GitHub push mirror has never completed a sync")
			}
			if mirror.LastError != "" {
				findings = append(findings, "GitHub push mirror reports a synchronization error (details withheld)")
			}
		}
	}
	if rule.Mirror == "required" && mirrorOK && !targetPresent {
		findings = append(findings, "required GitHub push mirror missing")
	}
	if rule.Retired {
		return findings, execution
	}
	if rule.DefaultBranch != "" && rule.DefaultBranch != repo.DefaultBranch {
		findings = append(findings, fmt.Sprintf("Forgejo default branch is %q, expected %q", repo.DefaultBranch, rule.DefaultBranch))
	}
	heads, protected, err := fetchAuditHeads(root+"/branches", token, false)
	if err != nil {
		execution = append(execution, "listing Forgejo heads: "+err.Error())
	}
	{
		protections := make(map[string]map[string]interface{}, len(rule.Protections))
		for branch, fields := range rule.Protections {
			protections[branch] = fields
		}
		// An unresolved class retains the active-main protection check, but does
		// not invent a main branch in master-only or dev-only repositories.
		if rule.Canonical == "review" {
			if _, ok := heads["main"]; ok {
				if _, declared := protections["main"]; !declared {
					protections["main"] = map[string]interface{}{}
				}
			}
		}
		if len(protections) > 0 {
			var actual []map[string]interface{}
			if err := fetchAuditProtections(root, token, &actual); err != nil {
				execution = append(execution, "reading branch protections: "+err.Error())
				// Branch metadata can prove an unprotected branch even when the full
				// admin contract is inaccessible. It cannot prove whitelist compliance.
				for branch := range protections {
					if value, ok := protected[branch]; ok && !value {
						findings = append(findings, branch+" branch protection missing (branch API)")
					}
				}
			} else {
				branches := make([]string, 0, len(protections))
				for branch := range protections {
					branches = append(branches, branch)
				}
				sort.Strings(branches)
				for _, branch := range branches {
					var protection map[string]interface{}
					for _, p := range actual {
						if p["rule_name"] == branch || p["branch_name"] == branch {
							protection = p
							break
						}
					}
					if protection == nil {
						findings = append(findings, branch+" branch protection missing")
						continue
					}
					fields := make([]string, 0, len(protections[branch]))
					for field := range protections[branch] {
						fields = append(fields, field)
					}
					sort.Strings(fields)
					for _, field := range fields {
						if !auditFieldMatches(field, protection[field], protections[branch][field]) {
							findings = append(findings, branch+" branch protection "+field+" differs from declared contract")
						}
					}
				}
			}
		}
	}
	if rule.Mirror == "forbidden" || rule.Mirror == "disabled" || rule.Mirror == "continuity" || rule.GitHub == "" {
		return findings, execution
	}
	var gh githubRepo
	ghRoot := "https://api.github.com/repos/" + rule.GitHub
	if err := auditGet(ghRoot, ghToken, true, &gh); err != nil {
		if errors.Is(err, auditHTTPError(404)) {
			if rule.Mirror == "optional" && !targetPresent {
				return findings, execution
			}
			findings = append(findings, "GitHub target absent or inaccessible (404); required or unresolved target remains unverified")
		} else {
			execution = append(execution, "reading GitHub target: "+err.Error())
		}
		return findings, execution
	}
	if rule.DefaultBranch != "" && gh.DefaultBranch != rule.DefaultBranch {
		findings = append(findings, fmt.Sprintf("GitHub default branch is %q, expected %q", gh.DefaultBranch, rule.DefaultBranch))
	}
	if rule.Canonical == "review" && mirrorOK && !targetPresent {
		findings = append(findings, "GitHub copy exists without an outgoing mirror; mirror purpose requires owner review")
	}
	ghHeads, _, ghErr := fetchAuditHeads(ghRoot+"/branches", ghToken, true)
	if ghErr != nil {
		execution = append(execution, "listing GitHub heads: "+ghErr.Error())
	} else if err == nil {
		for _, branch := range unionKeys(heads, ghHeads) {
			fg, fgOK := heads[branch]
			gh, ghOK := ghHeads[branch]
			switch {
			case !fgOK:
				findings = append(findings, fmt.Sprintf("branch %q exists only on GitHub", branch))
			case !ghOK:
				findings = append(findings, fmt.Sprintf("branch %q exists only on Forgejo", branch))
			case fg != gh:
				findings = append(findings, fmt.Sprintf("branch %q differs (Forgejo %s, GitHub %s)", branch, shortSHA(fg), shortSHA(gh)))
			}
		}
	}
	return findings, execution
}
func fetchAuditHeads(address, token string, github bool) (map[string]string, map[string]bool, error) {
	protected := map[string]bool{}
	heads := map[string]string{}
	for page := 1; ; page++ {
		var batch []struct {
			Name      string `json:"name"`
			Protected *bool  `json:"protected"`
			Commit    struct {
				ID  string `json:"id"`
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := auditGet(fmt.Sprintf("%s?limit=50&per_page=50&page=%d", address, page), token, github, &batch); err != nil {
			return nil, nil, err
		}
		if len(batch) == 0 {
			return heads, protected, nil
		}
		for _, b := range batch {
			sha := b.Commit.ID
			if github {
				sha = b.Commit.SHA
			}
			heads[b.Name] = sha
			if b.Protected != nil {
				protected[b.Name] = *b.Protected
			}
		}
	}
}
func auditFieldMatches(field string, actual, expected interface{}) bool {
	// Merge users are additive in the inventory contract. Push users, teams
	// and deploy-key access are exact so the automation exception stays narrow.
	a, _ := json.Marshal(actual)
	e, _ := json.Marshal(expected)
	if strings.HasSuffix(field, "usernames") || field == "status_check_contexts" || field == "approvals_whitelist_username" || field == "push_whitelist_teams" {
		var av, ev []string
		if json.Unmarshal(a, &av) != nil || json.Unmarshal(e, &ev) != nil {
			return false
		}
		sort.Strings(av)
		sort.Strings(ev)
		if field == "merge_whitelist_usernames" {
			for _, v := range ev {
				if !containsString(av, v) {
					return false
				}
			}
			return true
		}
		return strings.Join(av, "\x00") == strings.Join(ev, "\x00")
	}
	return string(a) == string(e)
}

// Forgejo returns the complete protection list without pagination. Unlike
// push_mirrors and branches, this endpoint has no page or limit parameters.
func fetchAuditProtections(root, token string, out *[]map[string]interface{}) error {
	return auditGet(root+"/branch_protections", token, false, out)
}
