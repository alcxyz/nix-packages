{
  buildGoModule,
  lib,
  makeWrapper,
  openssh,
  writableTmpDirAsHomeHook,
}:
buildGoModule (finalAttrs: {
  pname = "desk";
  # Bump with every change to the tool and record it in CHANGELOG.md.
  version = "0.1.0";

  src = ./.;

  vendorHash = null; # no external dependencies

  ldflags = [ "-X main.version=${finalAttrs.version}" ];

  # The go command writes telemetry under $HOME, which unsandboxed CI builds
  # would create as /homeless-shelter (#603).
  nativeBuildInputs = [
    writableTmpDirAsHomeHook
    makeWrapper
  ];

  postCheck = ''
    [ "$("$GOPATH/bin/desk" -version)" = "desk ${finalAttrs.version}" ]
  '';

  # Suffixed, so the user's own ssh, its configuration and agent come first.
  # The VNC viewer is the user's own (TigerVNC).
  postInstall = ''
    wrapProgram $out/bin/desk --suffix PATH : ${lib.makeBinPath [ openssh ]}
  '';

  meta = with lib; {
    description = "Start and end a remote-desk session on a host: SSH tunnel, remote-desk on, VNC viewer, remote-desk off";
    mainProgram = "desk";
    platforms = platforms.unix;
  };
})
