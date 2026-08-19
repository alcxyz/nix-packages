{
  callPackage,
  configFile ? "user",
  kernel ? null,
  nixpkgsPath,
}:

let
  generic = callPackage (nixpkgsPath + "/pkgs/os-specific/linux/zfs/generic.nix") {
    inherit configFile kernel;
  };
  genericArgs = generic.__functionArgs;
  kernelCompatibility =
    if genericArgs ? kernelMinSupportedMajorMinor then
      {
        kernelMinSupportedMajorMinor = "4.18";
        kernelMaxSupportedMajorMinor = "7.1";
      }
    else
      {
        kernelCompatible = kernel': kernel'.kernelAtLeast "4.18" && kernel'.kernelOlder "7.2";
      };
in
(generic (
  {
    version = "2.4.99-unstable-2026-06-17";
    rev = "a35e8d892628d01e50af23aee5ba501be426baf6";
    hash = "sha256-2QJdncmVHOJc2+K2Ygw5Eu/CLdsOAvumCmwjz9YKJB4=";

    kernelModuleAttribute = "openzfs_7_1";

    extraPatches = [ ];
    tests = { };

    extraLongDescription = ''
      This package pins the first upstream OpenZFS revision that declares Linux
      7.1 compatibility. It is an unreleased development snapshot intended for
      explicit, separately validated use on hosts that require Linux 7.1.
    '';
  }
  // kernelCompatibility
)).overrideAttrs
  (old: {
    # The development branch moved the sharing implementation after OpenZFS 2.4.
    # Keep using nixpkgs' generic builder while targeting the new source layout.
    postPatch =
      builtins.replaceStrings
        [
          "./lib/libshare/os/linux/nfs.c"
          "./lib/libshare/smb.h"
          "./cmd/arc_summary"
        ]
        [
          "./lib/libzfs/os/linux/libzfs_share_nfs.c"
          "./lib/libzfs/libzfs_share.h"
          "./cmd/zarcsummary"
        ]
        (old.postPatch or "");
  })
