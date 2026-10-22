{
  buildGoModule,
  lib,
  writableTmpDirAsHomeHook,
}:
buildGoModule (finalAttrs: {
  pname = "deck";
  version = "0.2.0";

  src = ./.;

  # The OIDC and JOSE dependencies are about 470 kB of third-party code, so
  # they are fetched by hash rather than committed to vendor/, where they would
  # dominate every review diff.
  vendorHash = "sha256-zaxaULgxvuCuq2NbS/dxqlxUzk2Ks6SVu+CCVGLcsRg=";

  # The go command writes telemetry under $HOME, which unsandboxed CI builds
  # would create as /homeless-shelter (#603). buildGoModule passes this on to
  # the module download too.
  nativeBuildInputs = [ writableTmpDirAsHomeHook ];

  ldflags = [ "-X main.version=${finalAttrs.version}" ];

  # The tests run the fake adapter commands in testdata/bin.
  postCheck = ''
    [ "$("$GOPATH/bin/deck" version)" = "deck ${finalAttrs.version}" ]
  '';

  # The T3 adapter commands and the model clients (claude, codex) come from
  # the consumer, which configures their paths or PATH.
  meta = {
    description = "Operator dashboard and interactive page collections";
    mainProgram = "deck";
    platforms = lib.platforms.unix;
  };
})
