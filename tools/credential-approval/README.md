# credential-approval

Waits for one person's passkey to approve one request.
[ADR-0018](../../docs/adr/0018-passkey-credential-approvals.md) records the
design. It uses only the Go standard library, so what verifies an approval is
all in this directory.

```sh
credential-approval wait \
  --spool /run/user/1000/credential-consent \
  --origin https://deck.example.com --rp-id deck.example.com \
  --public-key "$PINNED_KEY" --credential-id "$PINNED_ID" \
  --timeout 10m \
  --title "Short title" --reason "Why it is needed" \
  --impact "What it will do" --operation "label; SHA-256 ..." \
  --requester "Who asked" \
  --requester-url https://t3.example.com/thread \
  --notify /path/to/program
```

| Exit | Meaning |
| --- | --- |
| 0 | The pinned passkey approved this request |
| 1 | It could not run, for example an unwritable spool |
| 2 | Wrong usage or an unusable key |
| 3 | Denied |
| 4 | Expired or stopped by a signal, with no accepted answer |

`--public-key` is base64 SubjectPublicKeyInfo DER of an ES256 credential, as
the browser's `getPublicKey` gives it. `--notify` names a program run once as
`PROGRAM URL TITLE REQUESTER`, and its failure only logs a line.

## The spool

The page that relays answers runs as the same user and shares a directory with
this tool, private to that user. The two talk only through files named by the
request's ID:

| File | Written by | Holds |
| --- | --- | --- |
| `ID.request.json` | this tool | The request's exact bytes, which the page hashes and the passkey signs |
| `ID.response.json` | the page | `{"decision":"deny"}`, or `{"decision":"approve","assertion":{...}}` with `clientDataJSON`, `authenticatorData` and `signature` in unpadded base64url |
| `ID.rejected` | this tool | Nothing; its presence says the last approval did not verify |

Files are replaced by renaming, so neither side reads a partial one. The tool
removes a request's files when it ends, and sweeps expired ones when it starts.

## What an approval must be

- A `webauthn.get` assertion for the pinned origin, not cross-origin.
- Its challenge is the SHA-256 of this run's request bytes, which hold a fresh
  nonce and the text the person sees. The tool checks its own copy, not the
  spool's.
- For the pinned relying party, with the user present and verified.
- Signed by the pinned credential.

Anything else is marked rejected and the request stays open, so a bad answer
cannot end it. A deny is not signed. An answer read after the deadline never
counts.
