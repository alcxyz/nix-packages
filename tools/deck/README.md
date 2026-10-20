# deck

A live web dashboard of T3 Code threads: which ones wait on you, which have
stalled, which are ready to settle, and which settled threads may not be
done. See [ADR-0012](../../docs/adr/0012-deck-t3-thread-dashboard.md).

deck holds no queries against T3's database. It runs five adapter commands,
which the consumer installs, and parses their JSON:

| Command | Used for |
| --- | --- |
| `t3-thread-inventory --days N --db PATH` | the threads of the last N days |
| `t3-thread-status --db PATH ID...` | each thread's verdict, Settle line and idle time |
| `t3-thread-messages --db PATH --head N --tail N --max-chars N ID` | a thread's message revision, and its first and latest messages for a summary |
| `t3-thread-overview-group REPORT.json STATUS.json` | the overview's sections and counts |
| `t3-thread-overview-ack ID@SETTLED_AT` and `--forget ID` | accepting a settled thread as done, and undoing it |

Every 60 seconds (and when the page is opened on facts older than 15
seconds) deck refreshes each instance's facts. A failing or slow command never
blocks the page: it shows the last good facts, their time and the error.

Each thread that needs one gets a summary, cached in
`$XDG_STATE_HOME/deck/threads/summaries.json` against the thread's message
revision, bucket and settle time. A thread is summarised again only when one
of those changes and it has been quiet for `quietMinutes` (0 for no wait; it
need not wait once settled), by one model call through the shared `light`
role in `$XDG_CONFIG_HOME/llm/config.toml` (the format devlog reads; only the
`cli` transports are used), trying its backups in turn on failure. A failed
command or model call is retried after 15 minutes, doubling up to six hours;
invalid model output waits for the thread to change.
A worker runs `refreshConcurrency` calls at a time within `callsPerHour`. A
thread's "Refresh summary" button queues it first. Settled threads the facts
call done, and accepted ones, need no summary.

Summaries run only on a client that can run with no tools at all, which is
Claude Code; Codex entries of the role are skipped (logged at start), since
`codex exec` cannot turn off all of its tools, and so are `api` entries. The call is

```sh
claude -p --model M --effort E --safe-mode --restricted --tools "" \
  --strict-mcp-config --disable-slash-commands --no-session-persistence \
  --permission-mode dontAsk --output-format text
```

in an empty directory, with only `PATH`, `HOME`, `CLAUDE_CONFIG_DIR`,
`TMPDIR` and `LANG` from deck's environment. Every command and model call
runs in its own process group, killed as a whole at its deadline.

## Configuration

`deck serve -config PATH` (default `$XDG_CONFIG_HOME/deck/config.toml`):

```toml
listen = "127.0.0.1:3775"            # default
basePath = "/threads/"               # default
publicOrigin = "https://deck.example.com"

[auth]
issuer = "https://id.example.com"
clientID = "deck"
group = "deck-users"

[threads]
windowDays = 3           # default
quietMinutes = 10        # default; 0 for no quiet period
refreshConcurrency = 2   # default
callsPerHour = 60        # default

[commands]               # each defaults to its name on PATH
inventory = "t3-thread-inventory"
status = "t3-thread-status"
messages = "t3-thread-messages"
group = "t3-thread-overview-group"
ack = "t3-thread-overview-ack"

[[threads.instance]]
name = "main"                                   # [a-z0-9-]
label = "Main"                                  # optional
db = "/home/user/.t3/userdata/statev2.sqlite"
webURL = "https://t3.example.com"
# environmentID = "..."   # optional; overrides the inventory's environmentId
```

Thread links are `{webURL}/{environmentId}/{threadId}`.

## Authentication

Every route except `/healthz` needs `Authorization: Bearer <ID token>`, as an
oauth2-proxy sends it with `pass_authorization_header = true`. deck checks
the token itself against the issuer's discovery document and keys: issuer,
audience (`clientID`), expiry, and `group` in the `groups` claim. It answers
401 without a valid token and 403 without the group. POST requests also need
an `Origin` header equal to `publicOrigin`.

## Development

```sh
go test ./...
nix build .#deck
```

To try the page with the fixture data, point the commands at the fake ones
in `testdata/bin` and turn the token check off, which deck allows only on a
loopback address:

```toml
# dev.toml
listen = "127.0.0.1:3775"
publicOrigin = "http://127.0.0.1:3775"
[commands]
inventory = "/path/to/tools/deck/testdata/bin/t3-thread-inventory"
status = "/path/to/tools/deck/testdata/bin/t3-thread-status"
messages = "/path/to/tools/deck/testdata/bin/t3-thread-messages"
group = "/path/to/tools/deck/testdata/bin/t3-thread-overview-group"
ack = "/path/to/tools/deck/testdata/bin/t3-thread-overview-ack"
[[threads.instance]]
name = "main"
db = "/dev/null"
webURL = "https://t3.example.com"
```

Give it its own config and state directories, so it neither calls the
`light` role's real models nor shares the summary cache of a deck service
running as the same user. This works on Linux; on macOS Go reads the config
from `~/Library/Application Support` whatever `XDG_CONFIG_HOME` says, so run
it there with a `HOME` that has no `llm/config.toml`:

```sh
tmp=$(mktemp -d)
XDG_CONFIG_HOME=$tmp/config XDG_STATE_HOME=$tmp/state \
  go run . serve -config dev.toml -dev-no-auth
# open http://127.0.0.1:3775/threads/
```
