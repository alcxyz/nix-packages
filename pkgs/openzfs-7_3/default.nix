{
  callPackage,
  configFile ? "user",
  kernel ? null,
  # Also support userspacePackage.override { kernel = ...; configFile = "kernel"; }
  # outside linuxPackages.callPackage, retaining the kernel's build flags.
  kernelModuleMakeFlags ?
    if kernel == null then
      [ ]
    else
      kernel.commonMakeFlags
      ++ [ "KBUILD_OUTPUT=${kernel.dev}/lib/modules/${kernel.modDirVersion}/build" ],
  nixpkgsPath,
  tests ? { },
  util-linux,
}:

let
  sourceRevision = "cd3699d308e1b37dcdd0d5a9a8c3624deb5fc3e9";
  generic = callPackage (nixpkgsPath + "/pkgs/os-specific/linux/zfs/generic.nix") {
    inherit configFile kernel kernelModuleMakeFlags util-linux;
  };
  genericArgs = generic.__functionArgs;
  kernelCompatibility =
    if genericArgs ? kernelMinSupportedMajorMinor then
      {
        kernelMinSupportedMajorMinor = "4.18";
        kernelMaxSupportedMajorMinor = "7.3";
      }
    else
      {
        kernelCompatible = kernel': kernel'.kernelAtLeast "4.18" && kernel'.kernelOlder "7.4";
      };

  # The development branch moved sharing into libzfs and mounts snapshots in
  # the kernel instead of calling mount/umount, and renamed arc_summary. Adapt
  # those builder substitutions, including the module's userspaceTools passthru.
  adaptLayout =
    package:
    package.overrideAttrs (old: {
      postPatch =
        builtins.replaceStrings
          [
            "./lib/libshare/os/linux/nfs.c"
            "./lib/libshare/smb.h"
            "./cmd/arc_summary"
            ''
              # The arrays must remain the same length, so we repeat a flag that is
              # already part of the command and therefore has no effect.
              substituteInPlace ./module/os/linux/zfs/zfs_ctldir.c \
                --replace-fail '"/usr/bin/env", "umount"' '"${util-linux}/bin/umount", "-n"' \
                --replace-fail '"/usr/bin/env", "mount"'  '"${util-linux}/bin/mount", "-n"'
            ''
          ]
          [
            "./lib/libzfs/os/linux/libzfs_share_nfs.c"
            "./lib/libzfs/libzfs_share.h"
            "./cmd/zarcsummary"
            ""
          ]
          (old.postPatch or "");
      passthru = builtins.removeAttrs old.passthru [ "updateScript" ] // {
        userspaceTools = adaptLayout old.passthru.userspaceTools;
      };
      meta = old.meta // {
        changelog = "https://github.com/openzfs/zfs/commit/${sourceRevision}";
      };
    });
in
adaptLayout (
  generic (
    {
      version = "2.4.99-unstable-2026-09-10";
      # Final accepted commit of https://github.com/openzfs/zfs/pull/19082.
      # Contains bvec bi_offset, cpu_feature_enabled(), inode create without flag,
      # and scoped_with_init_fs() for SPL threads and taskqs. Do not float master.
      rev = sourceRevision;
      hash = "sha256-VX9G2Rn5PDUW/CxkY5u+hok9SntJLjCFgohhy7vVdao=";

      kernelModuleAttribute = "openzfs_7_3";

      # Upstream META still caps this snapshot at 7.2. This explicit local
      # candidate cap follows actual upstream compatibility fixes, keeps the
      # generic builder's META checks, and rejects future unsupported kernels.
      extraPatches = [ ./linux-7.3-candidate.patch ];
      inherit tests;

      extraLongDescription = ''
        This package pins an unreleased OpenZFS development snapshot containing
        the four accepted Linux 7.3 compatibility fixes from upstream PR 19082.
        Its Linux 7.3 maximum is a local candidate bound, build-validated on
        x86_64-linux, not an upstream stable support declaration. Runtime
        validation remains required. Use matching userspace and kernel modules;
        adoption does not require pool upgrades.
      '';
    }
    // kernelCompatibility
  )
)
