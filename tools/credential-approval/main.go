// credential-approval waits for one person's passkey to approve one request.
//
// It publishes a request in a spool directory, where a page relays it to the
// person and their answer back, and accepts an approval only if the pinned
// credential signed this request's exact bytes. See ADR-0018.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

// Exit codes, kept in step with the README.
const (
	exitApproved = 0
	exitError    = 1
	exitUsage    = 2
	exitDenied   = 3
	exitExpired  = 4
)

// pollInterval is how often the spool is checked for an answer.
const pollInterval = 500 * time.Millisecond

// notifyTimeout bounds the notify command, which must not hold up the answer.
const notifyTimeout = 30 * time.Second

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Println("credential-approval " + version)
		return exitApproved
	}
	if len(args) == 0 || args[0] != "wait" {
		fmt.Fprintln(os.Stderr, "usage: credential-approval wait --spool DIR --origin URL --rp-id ID --public-key KEY --credential-id ID --timeout DURATION --title T --reason R --impact I --operation O --requester Q [--requester-url URL] [--notify PROGRAM]")
		return exitUsage
	}
	flags := flag.NewFlagSet("wait", flag.ContinueOnError)
	spoolDir := flags.String("spool", "", "directory shared with the page")
	origin := flags.String("origin", "", "the page's origin, as the browser reports it")
	rpID := flags.String("rp-id", "", "the passkey's relying party ID")
	publicKey := flags.String("public-key", "", "the pinned credential's public key, base64 SubjectPublicKeyInfo")
	credentialID := flags.String("credential-id", "", "the pinned credential's ID, base64url")
	timeout := flags.Duration("timeout", 10*time.Minute, "how long the request stays open")
	title := flags.String("title", "", "short title")
	reason := flags.String("reason", "", "why it is needed")
	impact := flags.String("impact", "", "what it will do")
	operation := flags.String("operation", "", "operation label with its digest")
	requester := flags.String("requester", "", "who asked")
	requesterURL := flags.String("requester-url", "", "https link to the requester, for the person")
	notify := flags.String("notify", "", "program run once as PROGRAM URL TITLE REQUESTER")
	if err := flags.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if flags.NArg() != 0 || *spoolDir == "" || *credentialID == "" || *timeout <= 0 ||
		*title == "" || *reason == "" || *impact == "" || *operation == "" || *requester == "" {
		flags.Usage()
		return exitUsage
	}
	if *requesterURL != "" && !strings.HasPrefix(*requesterURL, "https://") {
		fmt.Fprintln(os.Stderr, "credential-approval: --requester-url must be https")
		return exitUsage
	}
	verifier, err := newVerifier(*publicKey, *rpID, *origin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "credential-approval:", err)
		return exitUsage
	}

	now := time.Now()
	deadline := now.Add(*timeout)
	request, err := newRequest(now, deadline, *rpID, *credentialID, *title, *reason, *impact, *operation, *requester, *requesterURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "credential-approval:", err)
		return exitError
	}
	requestBytes, err := json.Marshal(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, "credential-approval:", err)
		return exitError
	}
	if len(requestBytes) > maxRequestBytes {
		fmt.Fprintf(os.Stderr, "credential-approval: the request is longer than %d bytes\n", maxRequestBytes)
		return exitUsage
	}
	spool, err := openSpool(*spoolDir, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "credential-approval:", err)
		return exitError
	}
	defer spool.remove(request.ID)
	if err := spool.publish(requestBytes, request.ID); err != nil {
		fmt.Fprintln(os.Stderr, "credential-approval:", err)
		return exitError
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if *notify != "" {
		go runNotify(ctx, *notify, answerURL(*origin, request.ID), *title, *requester)
	}
	return waitForAnswer(ctx, deadline, spool, verifier, request.ID, requestBytes)
}

// runNotify tells the person there is something to answer. A failure is only
// logged: the page lists the request and the local prompt is up.
func runNotify(ctx context.Context, program, url, title, requester string) {
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	if err := exec.CommandContext(ctx, program, url, title, requester).Run(); err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		fmt.Fprintln(os.Stderr, "credential-approval: notify failed:", err)
	}
}

// waitForAnswer returns the exit code for the first deny or valid approval, or
// when the context ends. An answer that does not verify changes nothing, and
// one verified at or after the deadline, or after a signal, does not count. The
// clock is checked directly, since a context notices its deadline only when its
// timer runs.
func waitForAnswer(ctx context.Context, deadline time.Time, spool *Spool, verifier *Verifier, id string, requestBytes []byte) int {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		response, err := spool.readResponse(id)
		switch {
		case err != nil:
			fmt.Fprintln(os.Stderr, "credential-approval: ignoring a response:", err)
			spool.markRejected(id)
		case response != nil:
			code, answered := evaluate(response, verifier, requestBytes)
			if answered && ctx.Err() == nil && time.Now().Before(deadline) {
				return code
			}
			if !answered {
				spool.markRejected(id)
			}
		}
		select {
		case <-ctx.Done():
			return exitExpired
		case <-ticker.C:
		}
	}
}

func evaluate(response *Response, verifier *Verifier, requestBytes []byte) (code int, answered bool) {
	switch response.Decision {
	case "deny":
		return exitDenied, true
	case "approve":
		if response.Assertion == nil {
			fmt.Fprintln(os.Stderr, "credential-approval: an approval without an assertion")
			return 0, false
		}
		if err := verifier.verify(requestBytes, *response.Assertion); err != nil {
			fmt.Fprintln(os.Stderr, "credential-approval: rejected an approval:", err)
			return 0, false
		}
		return exitApproved, true
	default:
		fmt.Fprintf(os.Stderr, "credential-approval: unknown decision %q\n", response.Decision)
		return 0, false
	}
}
