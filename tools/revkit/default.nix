{
  buildGoModule,
  git,
  lib,
  makeWrapper,
}:
buildGoModule (finalAttrs: {
  pname = "revkit";
  # Bump with every change to the tool and record it in CHANGELOG.md.
  version = "0.1.1";

  src = ./.;

  vendorHash = null; # no external dependencies

  ldflags = [ "-X main.version=${finalAttrs.version}" ];

  nativeBuildInputs = [ makeWrapper ];
  nativeCheckInputs = [ git ];

  postCheck = ''
    [ "$("$GOPATH/bin/revkit" --version)" = "revkit ${finalAttrs.version}" ]
  '';

  # git writes the reviewed tree. The reviewer clients, claude and codex,
  # come from the consumer's PATH, which installs and updates them.
  postInstall = ''
    wrapProgram $out/bin/revkit --prefix PATH : ${lib.makeBinPath [ git ]}
  '';

  meta = with lib; {
    description = "Agent review toolkit: whole-repository reviews with sandboxed, isolated reviewers";
    mainProgram = "revkit";
    platforms = platforms.unix;
  };
})
