{
  git,
  lib,
  makeWrapper,
  python3,
  stdenvNoCC,
}:
stdenvNoCC.mkDerivation {
  pname = "git-identity-guard";
  version = "0.1.0";
  src = ./.;

  nativeBuildInputs = [ makeWrapper ];

  installPhase = ''
        install -Dm755 git-identity-guard.py "$out/libexec/git-identity-guard"
        substituteInPlace "$out/libexec/git-identity-guard" \
          --replace-fail '#!/usr/bin/env python3' '#!${python3}/bin/python3'
        makeWrapper "$out/libexec/git-identity-guard" "$out/bin/git-identity-guard" \
          --prefix PATH : ${lib.makeBinPath [ git ]}

        mkdir -p "$out/share/git-identity-guard/hooks"
        for hook in \
          applypatch-msg pre-applypatch post-applypatch \
          pre-commit pre-merge-commit prepare-commit-msg commit-msg post-commit \
          pre-rebase post-checkout post-merge pre-push \
          pre-receive update proc-receive post-receive post-update push-to-checkout \
          pre-auto-gc post-rewrite sendemail-validate fsmonitor-watchman \
          p4-changelist p4-prepare-changelist p4-post-changelist p4-pre-submit \
          post-index-change reference-transaction
        do
          cat > "$out/share/git-identity-guard/hooks/$hook" <<EOF
    #!${stdenvNoCC.shell}
    exec "$out/bin/git-identity-guard" hook "$hook" "\$@"
    EOF
          chmod 755 "$out/share/git-identity-guard/hooks/$hook"
        done
  '';

  meta = {
    description = "Git hook dispatcher and runtime commit email policy guard";
    mainProgram = "git-identity-guard";
    platforms = lib.platforms.unix;
  };
}
