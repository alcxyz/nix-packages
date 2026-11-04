{
  buildGoModule,
  lib,
  writableTmpDirAsHomeHook,
}:
buildGoModule (finalAttrs: {
  pname = "credential-approval";
  version = "0.2.0";

  src = ./.;

  # The standard library only, so what verifies an approval is all in this
  # directory.
  vendorHash = null;

  nativeBuildInputs = [ writableTmpDirAsHomeHook ];

  ldflags = [ "-X main.version=${finalAttrs.version}" ];

  meta = {
    description = "Wait for a passkey to approve one credential request";
    mainProgram = "credential-approval";
    platforms = lib.platforms.unix;
  };
})
