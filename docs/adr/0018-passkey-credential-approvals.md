# ADR-0018: Approve credential requests with a pinned passkey, relayed through deck

**Status:** Accepted
**Date:** 2026-11-04
**Applies to:** `tools/credential-approval/`, `tools/deck/`

## Context

A local authorization dialog can only be answered at the machine that shows it.
An operator away from that machine needs to answer the same question from a
laptop or phone, and the answer has to mean "this request", not "someone is
signed in here": a session proves who has a browser, and it could be replayed
or reused for a different request.

deck already serves operators over HTTPS behind an identity provider, so it is
a place to show the question. It should not become a place that decides it.

## Decision

`credential-approval wait` publishes a request and accepts an approval only if
one pinned passkey signed that request's exact bytes. It exits 0 for an
approval, and a caller treats any other result as no approval.

- The WebAuthn challenge is the SHA-256 of the request's JSON bytes. Those hold a
  fresh nonce, the expiry and the title, reason, impact, operation and requester.
- The tool checks its own in-memory copy of those bytes, the origin, the relying
  party, user presence and verification, and the signature against the public
  key given on its command line. Only P-256 (COSE ES256) keys are accepted.
- An answer that does not verify is marked rejected and the request stays open.
  Each run takes one answer and has its own nonce, so a signature cannot be
  reused.
- The tool uses only the Go standard library, and is a separate package from deck.

deck relays. With `[consent]` configured, it lists the requests in a spool
directory, shows one, and writes the browser's answer back as a file. It holds no
key and verifies no signature. The page's script takes the text it shows from
the same bytes it hashes for the passkey, so editing the request changes the
signature and fails. deck's existing checks apply: the identity provider's
token, read-only members cannot answer, and POST needs the configured origin.

`/consent/enroll` creates the passkey and shows its public key. deck stores
neither: the key counts only once the host's configuration pins it, so choosing
which passkey is trusted stays a reviewed configuration change.

## Alternatives Considered

- **Check the signed-in session.** This cannot bind an answer to one request, and
  anyone who can sign in could approve.
- **Verify in deck.** The verifying code would share a process and a dependency
  set with a web service, and deck would be trusted to decide rather than relay.
- **A server-side challenge kept by deck.** deck would need to be trusted with the
  link between what is shown and what is signed; hashing the shown bytes needs no
  such trust.
- **`go-webauthn` or a similar library.** Verifying one assertion for one pinned
  key needs a few checks and the standard library's ECDSA, and registration is
  not verified at all, since the operator pins the result by hand.

## Consequences

- An operator can answer from any device with the passkey, wherever deck is
  reachable. deck needs HTTPS at a stable host name, because a passkey is bound
  to it: moving deck to another host name means creating and pinning a new one.
- Replacing a lost passkey is a configuration change and a deploy.
- The relay must keep passing the bytes through unchanged.
- Callers own the fallback. A caller that needs an answer while deck or its route
  is down must offer another way to answer alongside this one.
