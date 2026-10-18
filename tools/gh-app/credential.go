package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// runCredential implements the Git credential helper protocol. It answers
// get only for installed repositories of the configured owner and otherwise
// stays silent, so Git asks its next helper. When the App cannot serve one
// of the owner's repositories it answers quit=1, so Git does not quietly
// fall back to the operator's credentials. It always exits 0.
func runCredential(cfg config, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "gh-app: usage: gh-app credential <get|store|erase>")
		return 0
	}
	attrs := readCredentialRequest(stdin)
	if args[0] != "get" || os.Getenv("GH_AS_OPERATOR") == "1" {
		return 0
	}
	configured, err := cfg.appConfigured()
	if !configured && err == nil {
		return 0
	}
	host := strings.ToLower(attrs["host"])
	host = strings.TrimSuffix(host, ":443")
	if attrs["protocol"] != "https" || host != githubHost {
		return 0
	}
	// A partial setup with a valid owner fails at minting below, so the
	// owner's repositories get quit=1 rather than the operator's login. With
	// an invalid owner no repository can be attributed, so every github.com
	// request stops instead of quietly using the operator's login.
	if err := cfg.checkOwner(); err != nil {
		fmt.Fprintf(stderr, "gh-app: %v; GH_AS_OPERATOR=1 uses the operator's credentials\n", err)
		fmt.Fprint(stdout, "quit=1\n")
		return 0
	}
	// owner/repo[.git], possibly followed by more, as in Git LFS requests.
	// Without a path (credential.useHttpPath off) the repository is unknown,
	// so stop Git rather than let the next helper answer for it.
	parts := strings.Split(strings.Trim(attrs["path"], "/"), "/")
	if len(parts) < 2 {
		fmt.Fprintln(stderr, "gh-app: no repository path in the credential request; set credential.https://github.com.useHttpPath=true, or GH_AS_OPERATOR=1 uses the operator's credentials")
		fmt.Fprint(stdout, "quit=1\n")
		return 0
	}
	ref, ok := newRepoRef(githubHost, parts[0], parts[1])
	if !ok || !strings.EqualFold(ref.Owner, cfg.owner) {
		return 0
	}
	entry, installed, err := newMinter(cfg).tokenFor(ref.Owner, ref.Name)
	if err != nil {
		fmt.Fprintf(stderr, "gh-app: no GitHub App token for %s/%s: %v; GH_AS_OPERATOR=1 uses the operator's credentials\n", ref.Owner, ref.Name, err)
		fmt.Fprint(stdout, "quit=1\n")
		return 0
	}
	if !installed {
		return 0
	}
	fmt.Fprintf(stdout, "username=x-access-token\npassword=%s\npassword_expiry_utc=%d\n", entry.Token, entry.ExpiresAt.Unix())
	return 0
}

// readCredentialRequest reads key=value lines until a blank line or EOF.
func readCredentialRequest(r io.Reader) map[string]string {
	attrs := map[string]string{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			break
		}
		key, value, found := strings.Cut(line, "=")
		if found {
			attrs[key] = value
		}
	}
	return attrs
}
