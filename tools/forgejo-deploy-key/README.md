# forgejo-deploy-key

Creates a write deploy key for one Forgejo repository and stores its private
half in a new SOPS file, without showing it in output, arguments or an
unencrypted file. It is the Forgejo counterpart of `gh-app register`, and
shares its SOPS store ([`lib/sopsstore`](../../lib/sopsstore)).

```sh
forgejo-deploy-key --repo OWNER/NAME --forgejo https://forgejo.example.org \
  --sops-file hosts/example/site-deploy-key.yaml --sops-key site_deploy_key
```

Run it from the repository holding `.sops.yaml`, with a path relative to it.
`--help` lists the other options (`--title`, `--ssh-host`, `--ssh-port`,
`--ssh-user`, `--known-hosts`, `--wait`).

## What it does

1. Refuses before creating anything if the SOPS file exists, its directory is
   missing, or `sops encrypt` would not encrypt a synthetic key there under
   `.sops.yaml`'s creation rules.
2. Asks `ssh -G` how to reach Forgejo's SSH server (host name, port,
   `ProxyCommand` or `ProxyJump`, `HostKeyAlias`, known hosts files), so a
   proxy configured there still works. The proxy runs with the user's own
   ssh configuration, agent and shell (`$SHELL`); a `ProxyJump` becomes
   `ssh -W` through the same hops, its last hop in batch mode.
3. Creates an ed25519 key in memory and adds it, on standard input, to a
   private `ssh-agent` whose socket is in a new `0700` directory. The key
   expires from the agent after the wait plus ten minutes, in case the
   command is killed.
4. Connects once with that key alone, ignoring the rest of the ssh
   configuration (`-F /dev/null`, `IdentitiesOnly`, `BatchMode`) and
   accepting only an already known host key (`StrictHostKeyChecking=yes`).
   An unknown host key, an unreachable server or a server that already
   accepts the new key stops it here, before the key is shown.
5. Prints the public key, its fingerprint and the repository's deploy key
   settings page. The operator adds the key there with write access.
6. Every ten seconds, until `--wait` (30 minutes) runs out, checks the key:
   - ssh without a command, where Forgejo answers whether it is a deploy key
     or a user's key. A user's key stops the command: it would carry that
     user's access;
   - `git-receive-pack 'OWNER/NAME.git'` given only a flush packet, where
     Forgejo checks write access as `git push --dry-run` would, and which
     ends without changing anything.

   A deploy key without write access is reported, and the command keeps
   waiting, as it does with any other refusal from Forgejo, such as a
   deploy key of another repository.
7. Writes the private key to the SOPS file with `sops encrypt`, checking that
   the output holds it encrypted and never in plaintext, and prints the file
   written.

The private key is passed only on standard input, to `ssh-add` and `sops`;
it never appears in output, arguments or an unencrypted file. `sops
encrypt` needs only the recipients' public keys, so an agent may run the
command. While it waits, though, the key is in the memory of the command
and of its `ssh-agent`, which processes of the same user, such as an agent
that started it, can reach. On a timeout, an interrupt or a failure after
the key was shown,
nothing is stored and the message names the fingerprint of the deploy key
to delete.

It needs `ssh`, `ssh-agent`, `ssh-add` and `sops` on `PATH`; the package
adds OpenSSH after the user's own.

The deploy key checks rely on Forgejo's SSH messages (also Gitea's), not on
its API: the account running the command needs no repository admin access.
