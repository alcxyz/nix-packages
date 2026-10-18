package main

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const githubHost = "github.com"

// repoRef names a repository on a host.
type repoRef struct {
	Host  string
	Owner string
	Name  string
}

var (
	namePattern    = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	apiRepoPattern = regexp.MustCompile(`^/?repos/([^/?#]+)/([^/?#]+)(?:[/?]|$)`)
	// gh fills these from the base repository, in both spellings.
	placeholderPattern = regexp.MustCompile(`:(owner|repo)\b|\{(owner|repo)\}`)
)

// appCommands are the gh commands that may act through the App. Every other
// command, and every unknown one, runs as the operator.
var appCommands = map[string]bool{
	"pr": true, "issue": true, "release": true, "run": true, "workflow": true,
	"label": true, "cache": true, "api": true, "repo": true,
}

// appSubcommands are the gh 2.96 subcommands of the App's command families
// (from their help). Anything else, such as an alias gh would expand, runs
// as the operator.
var appSubcommands = map[string]map[string]bool{
	"pr":       setOf("checkout checks close comment create diff edit list lock merge ready reopen revert review status unlock update-branch view"),
	"issue":    setOf("close comment create delete develop edit list lock pin reopen status transfer unlock unpin view"),
	"release":  setOf("create delete delete-asset download edit list upload verify verify-asset view"),
	"run":      setOf("cancel delete download list rerun view watch"),
	"workflow": setOf("disable enable list run view"),
	"label":    setOf("clone create delete edit list"),
	"cache":    setOf("delete list"),
}

// deployKeySubcommands are the children of gh 2.96's repo deploy-key.
var deployKeySubcommands = setOf("add delete list")

func setOf(words string) map[string]bool {
	set := map[string]bool{}
	for _, word := range strings.Fields(words) {
		set[word] = true
	}
	return set
}

// appRepoSubcommands are the gh repo subcommands that may act through the App.
// clone is not among them: it only reads, and its Git may fetch a fork's
// parent with the token.
var appRepoSubcommands = map[string]bool{
	"view": true, "edit": true, "sync": true, "archive": true,
	"unarchive": true, "rename": true, "delete": true, "set-default": true,
	"deploy-key": true,
}

// repoPositionalSubcommands take the repository as their first positional
// argument; rename takes a new name and deploy-key its own subcommand.
var repoPositionalSubcommands = map[string]bool{
	"view": true, "edit": true, "sync": true, "archive": true,
	"unarchive": true, "delete": true, "set-default": true,
}

var subcommandAliases = map[string]string{
	"pr new": "create", "pr ls": "list", "pr co": "checkout",
	"issue new": "create", "issue ls": "list",
	"release new": "create", "release ls": "list",
	"run ls": "list", "workflow ls": "list", "label ls": "list", "cache ls": "list",
}

// valueFlagSpecs lists, per command, the flags that take a value, from the
// help of gh 2.96. -R/--repo is accepted everywhere. An unlisted flag is
// read as a boolean, so an unknown valued flag can only make its value look
// positional; that value is then used only as a selector where gh reads one.
var valueFlagSpecs = map[string]string{
	"api": "--cache -F --field -H --header --hostname --input -q --jq -X --method -p --preview -f --raw-field -t --template",

	"pr create":            "-a --assignee -B --base -b --body -F --body-file -H --head -l --label -m --milestone -p --project --recover -r --reviewer -T --template -t --title",
	"pr list":              "--app -a --assignee -A --author -B --base -H --head -q --jq --json -l --label -L --limit -S --search -s --state -t --template",
	"pr status":            "-q --jq --json -t --template",
	"pr checkout":          "-b --branch",
	"pr checks":            "-i --interval -q --jq --json -t --template",
	"pr close":             "-c --comment",
	"pr comment":           "-b --body -F --body-file",
	"pr diff":              "--color -e --exclude",
	"pr edit":              "--add-assignee --add-label --add-project --add-reviewer -B --base -b --body -F --body-file -m --milestone --remove-assignee --remove-label --remove-project --remove-reviewer -t --title",
	"pr lock":              "-r --reason",
	"pr merge":             "-A --author-email -b --body -F --body-file --match-head-commit -t --subject",
	"pr reopen":            "-c --comment",
	"pr revert":            "-b --body -F --body-file -t --title",
	"pr review":            "-b --body -F --body-file",
	"pr view":              "-q --jq --json -t --template",
	"issue create":         "-a --assignee --blocked-by --blocking -b --body -F --body-file -l --label -m --milestone --parent -p --project --recover -T --template -t --title --type",
	"issue list":           "--app -a --assignee -A --author -q --jq --json -l --label -L --limit --mention -m --milestone -S --search -s --state -t --template --type",
	"issue status":         "-q --jq --json -t --template",
	"issue close":          "-c --comment --duplicate-of -r --reason",
	"issue comment":        "-b --body -F --body-file",
	"issue develop":        "-b --base --branch-repo -i --issue-repo -n --name",
	"issue edit":           "--add-assignee --add-blocked-by --add-blocking --add-label --add-project --add-sub-issue -b --body -F --body-file -m --milestone --parent --remove-assignee --remove-blocked-by --remove-blocking --remove-label --remove-project --remove-sub-issue -t --title --type",
	"issue lock":           "-r --reason",
	"issue reopen":         "-c --comment",
	"issue view":           "-q --jq --json -t --template",
	"release create":       "--discussion-category -n --notes -F --notes-file --notes-start-tag --target -t --title",
	"release list":         "-q --jq --json -L --limit -O --order -t --template",
	"release download":     "-A --archive -D --dir -O --output -p --pattern",
	"release edit":         "--discussion-category -n --notes -F --notes-file --tag --target -t --title",
	"release verify":       "--format -q --jq -t --template",
	"release verify-asset": "--format -q --jq -t --template",
	"release view":         "-q --jq --json -t --template",
	"run download":         "-D --dir -n --name -p --pattern",
	"run list":             "-b --branch -c --commit --created -e --event -q --jq --json -L --limit -s --status -t --template -u --user -w --workflow",
	"run rerun":            "-j --job",
	"run view":             "-a --attempt -j --job -q --jq --json -t --template",
	"run watch":            "-i --interval",
	"workflow list":        "-q --jq --json -L --limit -t --template",
	"workflow run":         "-F --field -f --raw-field -r --ref",
	"workflow view":        "-r --ref",
	"label create":         "-c --color -d --description",
	"label edit":           "-c --color -d --description -n --name",
	"label list":           "-q --jq --json -L --limit --order -S --search --sort -t --template",
	"cache delete":         "-r --ref",
	"cache list":           "-q --jq --json -k --key -L --limit -O --order -r --ref -S --sort -t --template",
	"repo view":            "-b --branch -q --jq --json -t --template",
	"repo clone":           "-u --upstream-remote-name",
	"repo edit":            "--add-topic --default-branch -d --description -h --homepage --remove-topic --squash-merge-commit-message --visibility",
	"repo sync":            "-b --branch -s --source",
	"repo deploy-key":      "--json -q --jq -t --template --title",
}

var (
	// valueFlags holds each subcommand's value flags, keyed like valueFlagSpecs.
	valueFlags = map[string]map[string]bool{}
	// familyValueFlags is the union for each command, used for flags given
	// before the subcommand.
	familyValueFlags = map[string]map[string]bool{}
	// longNames maps each short value flag to its long name, per subcommand
	// key and per command; a letter with different long names in one
	// command maps to "" there.
	longNames = map[string]map[string]string{}
)

func init() {
	for key, spec := range valueFlagSpecs {
		family, _, _ := strings.Cut(key, " ")
		if familyValueFlags[family] == nil {
			familyValueFlags[family] = map[string]bool{}
		}
		if longNames[family] == nil {
			longNames[family] = map[string]string{}
		}
		flags := map[string]bool{}
		names := map[string]string{}
		fields := strings.Fields(spec)
		for i, flag := range fields {
			flags[flag] = true
			familyValueFlags[family][flag] = true
			// The specs list a short flag right before its long name.
			if len(flag) == 2 && i+1 < len(fields) && strings.HasPrefix(fields[i+1], "--") {
				long := fields[i+1]
				names[flag] = long
				if previous, seen := longNames[family][flag]; seen && previous != long {
					long = ""
				}
				longNames[family][flag] = long
			}
		}
		valueFlags[key] = flags
		longNames[key] = names
	}
}

// invocation is a gh command line split into its parts.
type invocation struct {
	command     string
	sub         string
	repoFlag    string
	hasRepoFlag bool
	hostname    string
	positionals []string
	// values holds the last value of every flag given one, by its long name
	// where known.
	values map[string]string
	// every holds each value of a flag in order, for repeatable flags.
	every map[string][]string
	// switches holds the long flags given without a value.
	switches map[string]bool
	// afterDashes counts the positionals given after `--`.
	afterDashes int
	// flagBeforeCommand and flagBeforeSub are set by a flag other than
	// -R/--repo, --hostname, --help and --version given before the command,
	// or between an App command and its subcommand, where gh-app cannot tell
	// which values flags take.
	flagBeforeCommand, flagBeforeSub bool
	// afterSub is the word right after the subcommand, as given.
	afterSub    string
	sawAfterSub bool
}

// earlyFlags may stand before the subcommand: their meaning does not depend
// on it.
var earlyFlags = map[string]bool{"-R": true, "--repo": true, "--hostname": true, "--help": true, "--version": true}

// markEarly records a flag given before the command, or before the
// subcommand of an App command (api takes none).
func (inv *invocation) markEarly() {
	switch {
	case inv.command == "":
		inv.flagBeforeCommand = true
	case inv.sub == "" && appCommands[inv.command] && inv.command != "api":
		inv.flagBeforeSub = true
	}
}

// takesValue reports whether flag takes a value at this point of the
// command line: before the command only -R/--repo and --hostname are known,
// before the subcommand any value flag of the command's subcommands.
func (inv invocation) takesValue(flag string) bool {
	switch {
	case flag == "-R" || flag == "--repo":
		return true
	case inv.command == "":
		return flag == "--hostname"
	case inv.command == "api":
		return valueFlags["api"][flag]
	case inv.sub == "":
		return familyValueFlags[inv.command][flag]
	}
	return valueFlags[inv.command+" "+inv.sub][flag]
}

// longName returns the long spelling of a short flag where it is known, so
// the last occurrence of either spelling wins, as in gh.
func (inv invocation) longName(flag string) string {
	if flag == "-R" {
		return "--repo"
	}
	key := inv.command
	if inv.command != "api" && inv.sub != "" {
		key = inv.command + " " + inv.sub
	}
	if long := longNames[key][flag]; long != "" {
		return long
	}
	return flag
}

func (inv *invocation) setFlag(flag, value string) {
	flag = inv.longName(flag)
	inv.values[flag] = value
	inv.every[flag] = append(inv.every[flag], value)
	switch flag {
	case "--repo":
		// An empty value counts as not given, as in gh.
		inv.repoFlag = value
		inv.hasRepoFlag = value != ""
	case "--hostname":
		inv.hostname = value
	}
}

// parseInvocation splits gh's arguments into the command, the subcommand,
// flags and positional arguments, wherever the flags appear. Flag values are
// never positional, --flag=value never takes the next argument, and "--"
// ends the flags.
func parseInvocation(args []string) invocation {
	inv := invocation{values: map[string]string{}, every: map[string][]string{}, switches: map[string]bool{}}
	flagsDone := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if inv.sub != "" && !inv.sawAfterSub {
			inv.afterSub, inv.sawAfterSub = arg, true
		}
		switch {
		case flagsDone || arg == "-" || !strings.HasPrefix(arg, "-"):
			switch {
			case inv.command == "":
				inv.command = arg
			case inv.command != "api" && inv.sub == "":
				inv.sub = arg
				if alias, ok := subcommandAliases[inv.command+" "+arg]; ok {
					inv.sub = alias
				}
			default:
				inv.positionals = append(inv.positionals, arg)
				if flagsDone {
					inv.afterDashes++
				}
			}
		case arg == "--":
			flagsDone = true
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg, "=")
			if !earlyFlags[name] {
				inv.markEarly()
			}
			if !hasValue && inv.takesValue(name) && i+1 < len(args) {
				i++
				value = args[i]
				hasValue = true
			}
			if hasValue {
				inv.setFlag(name, value)
			} else {
				inv.switches[name] = true
			}
		default:
			// Before the subcommand only -R may stand, with its value
			// attached or next.
			if !strings.HasPrefix(arg, "-R") {
				inv.markEarly()
			}
			// A cluster of short flags; the first one that takes a value
			// takes the rest of the cluster or the next argument.
			for j := 1; j < len(arg); j++ {
				flag := "-" + arg[j:j+1]
				if !inv.takesValue(flag) {
					continue
				}
				if j+1 < len(arg) {
					inv.setFlag(flag, strings.TrimPrefix(arg[j+1:], "="))
				} else if i+1 < len(args) {
					i++
					inv.setFlag(flag, args[i])
				}
				break
			}
		}
	}
	return inv
}

// newRepoRef checks GitHub's owner and repository name rules on github.com
// only; on other hosts any non-empty segments parse, and the host alone then
// rules the repository out.
func newRepoRef(host, owner, name string) (repoRef, bool) {
	name = strings.TrimSuffix(name, ".git")
	host = normalizeHost(host)
	if host == "" || owner == "" || name == "" {
		return repoRef{}, false
	}
	if host == githubHost && (!ownerPattern.MatchString(owner) || !namePattern.MatchString(name) || name == "." || name == "..") {
		return repoRef{}, false
	}
	return repoRef{Host: host, Owner: owner, Name: name}, true
}

// parseRepoSpec parses a repository selector: OWNER/REPO (on defaultHost),
// HOST/OWNER/REPO or a URL.
func parseRepoSpec(spec, defaultHost string) (repoRef, bool) {
	if strings.Contains(spec, "://") {
		return parseRepoURL(spec)
	}
	parts := strings.Split(strings.Trim(spec, "/"), "/")
	switch len(parts) {
	case 2:
		return newRepoRef(defaultHost, parts[0], parts[1])
	case 3:
		return newRepoRef(parts[0], parts[1], parts[2])
	}
	return repoRef{}, false
}

// parseRepoURL parses http(s) and ssh URLs whose path starts with OWNER/REPO.
func parseRepoURL(raw string) (repoRef, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return repoRef{}, false
	}
	switch u.Scheme {
	case "https", "http", "ssh", "git+ssh", "ssh+git":
	default:
		return repoRef{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return repoRef{}, false
	}
	return newRepoRef(u.Hostname(), parts[0], parts[1])
}

func isWebURL(arg string) bool {
	lower := strings.ToLower(arg)
	return strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")
}

// parseRemoteURL parses a Git remote URL, including the scp-like form, as
// gh's ghrepo.FromURL does: the path must be exactly OWNER/REPO (with an
// optional .git and trailing slash). Selector URLs use parseRepoURL instead,
// which also accepts longer paths such as pull request URLs.
func parseRemoteURL(raw string) (repoRef, bool) {
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return repoRef{}, false
		}
		// The schemes gh 2.96's git.ParseURL accepts with a host.
		switch strings.ToLower(u.Scheme) {
		case "https", "http", "git", "git+https", "ssh", "git+ssh", "ftp", "ftps":
		default:
			return repoRef{}, false
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) != 2 {
			return repoRef{}, false
		}
		return newRepoRef(u.Hostname(), parts[0], parts[1])
	}
	// scp-like: [user@]host:OWNER/REPO
	colon := strings.IndexByte(raw, ':')
	if colon < 0 || strings.Contains(raw[:colon], "/") {
		return repoRef{}, false
	}
	host := raw[:colon]
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	parts := strings.Split(strings.Trim(raw[colon+1:], "/"), "/")
	if len(parts) != 2 {
		return repoRef{}, false
	}
	return newRepoRef(host, parts[0], parts[1])
}

// resolveTarget finds the repository a gh invocation acts on, with gh's own
// precedence. ok is false when there is no target or the command is not one
// the App may run.
func resolveTarget(args []string, getenv func(string) string, dir string) (repoRef, bool) {
	inv := parseInvocation(args)
	ref, ok := resolveInvocation(inv, getenv, dir)
	if !ok || crossRepository(inv, ref, dir) {
		return repoRef{}, false
	}
	return ref, true
}

func resolveInvocation(inv invocation, getenv func(string) string, dir string) (repoRef, bool) {
	if !appCommands[inv.command] {
		return repoRef{}, false
	}
	if inv.command == "repo" && !appRepoSubcommands[inv.sub] {
		return repoRef{}, false
	}
	// deploy-key is a group; an alias beneath it would expand elsewhere, and
	// a flag before its child could take the child as its value, so the
	// child must come right after it.
	if inv.command == "repo" && inv.sub == "deploy-key" && !deployKeySubcommands[inv.afterSub] {
		return repoRef{}, false
	}
	if subs, ok := appSubcommands[inv.command]; ok && !subs[inv.sub] {
		return repoRef{}, false
	}
	defaultHost := githubHost
	if host := getenv("GH_HOST"); host != "" {
		defaultHost = strings.ToLower(host)
	}

	if inv.command == "api" {
		return apiTarget(inv, getenv, dir, defaultHost)
	}

	// Pull request and issue URLs select their repository, even over -R.
	// URLs of different repositories leave the command to the operator.
	if inv.command == "pr" || inv.command == "issue" {
		var selected repoRef
		found := false
		for _, arg := range inv.positionals {
			if !isWebURL(arg) {
				continue
			}
			ref, ok := parseRepoURL(arg)
			if !ok {
				continue
			}
			if !found {
				selected, found = ref, true
			} else if !sameRepo(ref, selected) {
				return repoRef{}, false
			}
		}
		if found {
			return selected, true
		}
	}
	if inv.command == "issue" && inv.sub == "develop" && inv.values["--issue-repo"] != "" {
		// gh applies --issue-repo as the issue repository override.
		return parseRepoSpec(inv.values["--issue-repo"], defaultHost)
	}
	if inv.hasRepoFlag {
		return parseRepoSpec(inv.repoFlag, defaultHost)
	}
	if inv.command == "repo" && repoPositionalSubcommands[inv.sub] && len(inv.positionals) > 0 {
		selector := inv.positionals[0]
		switch {
		case strings.Contains(selector, ":"):
			// A URL, or an scp-style Git address.
			ref, ok, unknown := resolveRemoteURL(selector)
			return ref, ok && !unknown
		case !strings.Contains(selector, "/"):
			// A bare name is the authenticated account's repository,
			// which gh-app cannot know without asking GitHub.
			return repoRef{}, false
		}
		return parseRepoSpec(selector, defaultHost)
	}
	if inv.command == "repo" && repoPositionalSubcommands[inv.sub] {
		// In gh 2.96 only rename and deploy-key among the repo subcommands
		// honour GH_REPO; the others use the checkout.
		if defaultHost != githubHost {
			return repoRef{}, false
		}
		return remoteTarget(dir)
	}
	return baseRepo(getenv, dir, defaultHost)
}

// viewerFlags are the flags whose values gh expands @me in; free text such
// as --body or --title is not among them.
var viewerFlags = map[string]bool{
	"--author": true, "--assignee": true, "--add-assignee": true, "--remove-assignee": true,
	"--reviewer": true, "--add-reviewer": true, "--remove-reviewer": true, "--mention": true, "--search": true,
}

// viewerShortFlags are the short spellings of viewer flags in some
// subcommand: -A --author, -a --assignee, -r --reviewer, -S --search.
var viewerShortFlags = map[string]bool{"-A": true, "-a": true, "-r": true, "-S": true}

// viewerToken matches @me as a whole token, as in `@me`, `author:@me` or a
// CSV-quoted `"@me"`.
var viewerToken = regexp.MustCompile(`(^|[\s:,"'])@me($|[\s,"'])`)

// crossRepository reports a command that also acts on a second repository,
// which the App's token may not cover; such commands run as the operator.
func crossRepository(inv invocation, target repoRef, dir string) bool {
	key := inv.command + " " + inv.sub
	// An installation token has no user, so gh cannot look up the viewer
	// (status views, @me); those commands are about the operator anyway.
	if inv.sub == "status" && (inv.command == "pr" || inv.command == "issue") {
		return true
	}
	for flag, values := range inv.every {
		if inv.command != "pr" && inv.command != "issue" {
			break
		}
		// A short flag given before the subcommand may stay unresolved
		// (-A is --author in pr list); the letters that are a user filter in
		// some subcommand count like the viewer flags.
		if !viewerFlags[flag] && !viewerShortFlags[flag] {
			continue
		}
		for _, value := range values {
			if viewerToken.MatchString(value) {
				return true
			}
		}
	}
	// gh runs Git in the checkout with the token for these; in a checkout
	// with submodules, Git may fetch or push them with it too.
	switch key {
	case "pr create", "pr merge", "issue develop":
		if hasSubmodules(dir) {
			return true
		}
	}
	switch key {
	case "issue transfer", "label clone":
		return true
	case "pr update-branch":
		// GitHub requires write access to the head repository, which may be
		// a fork outside the installation.
		return true
	case "issue close":
		for _, value := range inv.every["--duplicate-of"] {
			if strings.Contains(value, "/") {
				return true
			}
		}
		return false
	case "pr checkout":
		// Local Git work that only reads: nothing is attributed, and gh's Git
		// may fetch other repositories with the token (a fork head,
		// submodules), so it always runs as the operator, like repo clone.
		return true
	case "issue create", "issue edit":
		// Relationship flags may name an issue in another repository.
		for _, flag := range []string{"--parent", "--add-sub-issue", "--remove-sub-issue", "--blocked-by", "--blocking",
			"--add-blocked-by", "--add-blocking", "--remove-blocked-by", "--remove-blocking"} {
			for _, value := range inv.every[flag] {
				if strings.Contains(value, "/") {
					return true
				}
			}
		}
		return false
	case "repo sync":
		if _, ok := inv.values["--source"]; ok {
			return true
		}
		// Without a repository it syncs the local checkout, which only reads
		// on GitHub; like repo clone it runs as the operator.
		return len(inv.positionals) == 0
	case "issue develop":
		if _, ok := inv.values["--branch-repo"]; ok {
			return true
		}
		// When both flags are given, even empty, gh rewrites -R into
		// --branch-repo.
		_, issueRepo := inv.values["--issue-repo"]
		_, repo := inv.values["--repo"]
		return issueRepo && repo
	case "pr create":
		head, ok := inv.values["--head"]
		if !ok || head == "" {
			// gh picks the head itself, and pushes to a fork when the checkout
			// has one; an empty --head= is the same.
			return forkCheckout(dir, target)
		}
		owner, _, isFork := strings.Cut(head, ":")
		if !isFork {
			return false
		}
		if strings.Contains(owner, "/") {
			// [HOST/]OWNER/REPO:BRANCH
			ref, ok := parseRepoSpec(owner, target.Host)
			return !ok || !sameRepo(ref, target)
		}
		return !strings.EqualFold(owner, target.Owner)
	}
	return false
}

func sameRepo(a, b repoRef) bool {
	return a.Host == b.Host && strings.EqualFold(a.Owner, b.Owner) && strings.EqualFold(a.Name, b.Name)
}

// apiTarget classifies a gh api endpoint, after filling {owner}, {repo}, :owner and :repo
// from the base repository as gh does. Endpoints outside repos/ have no
// target; absolute URLs count on api.github.com and on /api/v3/ of a host.
func apiTarget(inv invocation, getenv func(string) string, dir, defaultHost string) (repoRef, bool) {
	host := defaultHost
	if inv.hostname != "" {
		host = strings.ToLower(inv.hostname)
	}
	if len(inv.positionals) == 0 {
		return repoRef{}, false
	}
	endpoint := inv.positionals[0]
	if placeholderPattern.MatchString(endpoint) {
		base, ok := baseRepo(getenv, dir, defaultHost)
		if !ok {
			return repoRef{}, false
		}
		endpoint = placeholderPattern.ReplaceAllStringFunc(endpoint, func(placeholder string) string {
			if strings.Contains(placeholder, "owner") {
				return base.Owner
			}
			return base.Name
		})
	}
	if isWebURL(endpoint) {
		u, err := url.Parse(endpoint)
		if err != nil {
			return repoRef{}, false
		}
		urlHost := strings.ToLower(u.Hostname())
		switch {
		case urlHost == "api."+githubHost:
			host = githubHost
			endpoint = u.Path
		case strings.HasPrefix(u.Path, "/api/v3/"):
			host = urlHost
			endpoint = strings.TrimPrefix(u.Path, "/api/v3")
		default:
			return repoRef{}, false
		}
	}
	match := apiRepoPattern.FindStringSubmatch(endpoint)
	if match == nil {
		return repoRef{}, false
	}
	return newRepoRef(host, match[1], match[2])
}

// baseRepo is the repository gh uses when none is named: GH_REPO, else the
// checkout's remotes, which count only on github.com.
func baseRepo(getenv func(string) string, dir, defaultHost string) (repoRef, bool) {
	if spec := getenv("GH_REPO"); spec != "" {
		return parseRepoSpec(spec, defaultHost)
	}
	if defaultHost != githubHost {
		return repoRef{}, false
	}
	return remoteTarget(dir)
}

// ghPlan is the process gh-app replaces itself with.
type ghPlan struct {
	path      string
	argv      []string
	env       []string
	withToken bool
}

// planGH decides how to run gh: with the App's token for an installed
// repository of the configured owner, otherwise unchanged.
func planGH(cfg config, args, env []string, dir string, m *minter) (ghPlan, error) {
	if err := cfg.checkGH(); err != nil {
		return ghPlan{}, err
	}
	plain := ghPlan{path: cfg.ghPath, argv: append([]string{cfg.ghPath}, args...), env: env}
	if lookupEnv(env, "GH_AS_OPERATOR") == "1" {
		return plain, nil
	}
	configured, err := cfg.appConfigured()
	if err != nil {
		return ghPlan{}, err
	}
	if !configured {
		return plain, nil
	}
	if err := cfg.checkOwner(); err != nil {
		return ghPlan{}, err
	}
	// A flag before the command may take the next word as its value, so the
	// command itself is uncertain; refuse whatever command follows.
	if inv := parseInvocation(args); inv.flagBeforeCommand || inv.flagBeforeSub {
		return ghPlan{}, errors.New("give flags after the subcommand (gh <command> <subcommand> [flags]), so the repository the command acts on is clear; GH_AS_OPERATOR=1 runs the command as the operator")
	}
	getenv := func(key string) string { return lookupEnv(env, key) }
	target, ok := resolveTarget(args, getenv, dir)
	if !ok || target.Host != githubHost || !strings.EqualFold(target.Owner, cfg.owner) {
		return plain, nil
	}
	entry, installed, err := m.tokenFor(target.Owner, target.Name)
	if err != nil {
		return ghPlan{}, fmt.Errorf("no GitHub App token for %s/%s: %v; GH_AS_OPERATOR=1 runs the command as the operator", target.Owner, target.Name, err)
	}
	if !installed {
		return plain, nil
	}
	var tokenEnv []string
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN":
			continue
		}
		tokenEnv = append(tokenEnv, kv)
	}
	tokenEnv = append(tokenEnv, "GH_TOKEN="+entry.Token)
	return ghPlan{path: cfg.ghPath, argv: plain.argv, env: tokenEnv, withToken: true}, nil
}

func lookupEnv(env []string, key string) string {
	value := ""
	for _, kv := range env {
		k, v, found := strings.Cut(kv, "=")
		if found && k == key {
			value = v
		}
	}
	return value
}
