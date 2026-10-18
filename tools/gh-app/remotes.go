package main

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// hasSubmodules reports whether the checkout containing dir may have
// submodules Git fetches: a .gitmodules file in the working tree or in HEAD,
// or submodule repositories under the Git directory.
func hasSubmodules(dir string) bool {
	if dir == "" {
		return false
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel", "--git-dir", "--git-common-dir").Output()
	if err != nil {
		return false
	}
	paths := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(paths) != 3 {
		return true // cannot tell; Git may fetch submodules
	}
	if _, err := os.Stat(filepath.Join(paths[0], ".gitmodules")); err == nil {
		return true
	}
	// A linked worktree keeps its submodule repositories under its own Git
	// directory, the main checkout under the common one.
	for _, gitDir := range paths[1:] {
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(dir, gitDir)
		}
		if _, err := os.Stat(filepath.Join(gitDir, "modules")); err == nil {
			return true
		}
	}
	return exec.Command("git", "-C", dir, "cat-file", "-e", "HEAD:.gitmodules").Run() == nil
}

// normalizeHost lowercases a host and reads gh's GitHub aliases
// (www.github.com, and ssh.github.com for SSH over port 443) as github.com.
func normalizeHost(host string) string {
	host = strings.ToLower(host)
	switch host {
	case "www." + githubHost, "ssh." + githubHost:
		return githubHost
	}
	return host
}

func isSSHAddress(raw string) bool {
	lower := strings.ToLower(raw)
	if !strings.Contains(lower, "://") {
		return true
	}
	return strings.HasPrefix(lower, "ssh://") || strings.HasPrefix(lower, "git+ssh://")
}

// sshHostname resolves an SSH host alias with ssh -G, which only reads the
// SSH configuration. ok is false when ssh is missing or fails.
func sshHostname(alias string) (string, bool) {
	if alias == "" || strings.HasPrefix(alias, "-") {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-G", alias).Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.EqualFold(fields[0], "hostname") {
			return normalizeHost(fields[1]), true
		}
	}
	return "", false
}

// resolveRemoteURL parses a Git remote URL and resolves the host of an SSH
// address through the SSH configuration, as SSH would connect. unknown is
// true when that host could not be resolved.
func resolveRemoteURL(raw string) (ref repoRef, ok bool, unknown bool) {
	ref, ok = parseRemoteURL(raw)
	if !ok || ref.Host == githubHost || !isSSHAddress(raw) {
		return ref, ok, false
	}
	host, found := sshHostname(sshAlias(raw))
	if !found {
		return ref, true, true
	}
	ref.Host = host
	return ref, true, false
}

// remote is a Git remote whose fetch URL names a repository.
type remote struct {
	name    string
	ref     repoRef
	unknown bool
}

// remotePriority is gh's remote order: upstream, github, origin, then the
// rest in git remote order.
func remotePriority(name string) int {
	switch strings.ToLower(name) {
	case "upstream":
		return 0
	case "github":
		return 1
	case "origin":
		return 2
	}
	return 3
}

// listRemotes reads the remotes' effective fetch URLs (after
// url.*.insteadOf) in gh's order.
func listRemotes(dir string) []remote {
	if dir == "" {
		return nil
	}
	cmd := exec.Command("git", "remote", "-v")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var names []string
	fetchURLs := map[string]string{}
	pushURLs := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		// A partial clone's fetch line ends with its filter, as in
		// `origin <url> (fetch) [blob:none]`.
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		name := fields[0]
		if _, seen := fetchURLs[name]; !seen {
			if _, seen := pushURLs[name]; !seen {
				names = append(names, name)
			}
		}
		switch fields[2] {
		case "(fetch)":
			if _, seen := fetchURLs[name]; !seen {
				fetchURLs[name] = fields[1]
			}
		case "(push)":
			// Like gh, the last push URL of a remote counts.
			pushURLs[name] = fields[1]
		}
	}
	var remotes []remote
	for _, name := range names {
		ref, ok, unknown := resolveRemoteURL(fetchURLs[name])
		// Like gh, fall back to the push URL only when the fetch URL does
		// not parse as a repository URL; one on another host stays there.
		if !ok {
			ref, ok, unknown = resolveRemoteURL(pushURLs[name])
		}
		if ok {
			remotes = append(remotes, remote{name: name, ref: ref, unknown: unknown})
		}
	}
	sort.SliceStable(remotes, func(i, j int) bool {
		return remotePriority(remotes[i].name) < remotePriority(remotes[j].name)
	})
	return remotes
}

// remoteTarget picks the GitHub repository gh would use from the remotes, in
// gh's order: the first remote with a gh-resolved setting (an explicit
// OWNER/REPO, or base for the remote's own repository), else the first
// github.com remote. A remote whose SSH host cannot be resolved and that
// could come first leaves the choice open: no target.
func remoteTarget(dir string) (repoRef, bool) {
	remotes := listRemotes(dir)
	if len(remotes) == 0 {
		return repoRef{}, false
	}

	resolved := map[string]string{}
	cmd := exec.Command("git", "config", "--get-regexp", `^remote\..*\.gh-resolved$`)
	cmd.Dir = dir
	// No gh-resolved setting makes git config exit 1.
	out, _ := cmd.Output()
	for _, line := range strings.Split(string(out), "\n") {
		key, value, found := strings.Cut(line, " ")
		if found && strings.HasPrefix(key, "remote.") && strings.HasSuffix(key, ".gh-resolved") {
			resolved[strings.TrimSuffix(strings.TrimPrefix(key, "remote."), ".gh-resolved")] = value
		}
	}
	for _, r := range remotes {
		value := resolved[r.name]
		if value == "" {
			continue
		}
		if r.unknown {
			return repoRef{}, false
		}
		if r.ref.Host != githubHost {
			continue
		}
		if value == "base" {
			return r.ref, true
		}
		if spec, found := parseRepoSpec(value, githubHost); found {
			return spec, true
		}
	}
	return firstGitHubRemote(remotes)
}

// firstGitHubRemote is the first github.com remote in gh's priority order
// (upstream, github, origin, then the rest); a remote whose host is unknown
// and comes first leaves the choice open.
func firstGitHubRemote(remotes []remote) (repoRef, bool) {
	for _, r := range remotes {
		if r.unknown {
			return repoRef{}, false
		}
		if r.ref.Host == githubHost {
			return r.ref, true
		}
	}
	return repoRef{}, false
}

// forkCheckout reports a checkout with a GitHub remote of another owner than
// target's, or a remote whose host is unknown, where gh pr create would push
// to a fork.
func forkCheckout(dir string, target repoRef) bool {
	for _, r := range listRemotes(dir) {
		if r.unknown || r.ref.Host == githubHost && !strings.EqualFold(r.ref.Owner, target.Owner) {
			return true
		}
	}
	return false
}

// sshAlias returns the host of an SSH address as written: OpenSSH matches
// Host patterns case-sensitively.
func sshAlias(raw string) string {
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return u.Hostname()
	}
	host, _, _ := strings.Cut(raw, ":")
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	return host
}
