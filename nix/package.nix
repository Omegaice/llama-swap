# Adapted from nixpkgs's pkgs/by-name/ll/llama-swap/package.nix: the source
# and its identity come from the flake instead of a GitHub fetch, so a commit
# here updates the build in the same change.
{
  lib,
  stdenv,
  buildGoModule,
  callPackage,
  versionCheckHook,
  bash,

  src,
  rev,
  lastModifiedDate,
  withUI ? true,
}:
let
  canExecute = stdenv.buildPlatform.canExecute stdenv.hostPlatform;

  # The flake's YYYYMMDDHHMMSS as the RFC 3339 stamp upstream's release
  # builds put in main.date.
  date =
    let
      s = lastModifiedDate;
      sub = builtins.substring;
    in
    "${sub 0 4 s}-${sub 4 2 s}-${sub 6 2 s}T${sub 8 2 s}:${sub 10 2 s}:${sub 12 2 s}Z";
in
buildGoModule (finalAttrs: {
  pname = "llama-swap";
  # Upstream tag the fork is based on, marked so a listing or /api/version
  # never passes it off as a stock release.
  version = "249-fleet";

  inherit src;

  outputs = [
    "out"
    "wol" # wake on lan proxy
  ];

  vendorHash = "sha256-MhR8B2+Yb/xqrTlIxaVHLoQf1eTOO49c65l72IAuZyU=";

  # Upstream only embeds the UI when this build tag is set.
  tags = lib.optionals withUI [ "embed_ui" ];

  passthru.ui = callPackage ./ui.nix { llama-swap = finalAttrs.finalPackage; };

  nativeBuildInputs = [ versionCheckHook ];

  # required for testing
  __darwinAllowLocalNetworking = true;

  ldflags = [
    "-s"
    "-w"
    "-X main.version=${finalAttrs.version}"
    "-X main.commit=${rev}"
    "-X main.date=${date}"
  ];

  postPatch = ''
    substituteInPlace internal/process/process_command_forking_test.go \
      --replace-fail "#!/bin/bash" "#!${lib.getExe bash}"
  '';

  preBuild = lib.optionalString withUI ''
    # copy for go:embed in internal/server/embed.go
    cp -r ${finalAttrs.passthru.ui}/ui_dist internal/server/
  '';

  checkFlags =
    let
      skippedTests = lib.optionals stdenv.hostPlatform.isDarwin [
        # Fail only on *-darwin intermittently
        # https://github.com/mostlygeek/llama-swap/issues/320
        "TestProcess_AutomaticallyStartsUpstream"
        "TestProcess_WaitOnMultipleStarts"
        "TestProcess_BrokenModelConfig"
        "TestProcess_UnloadAfterTTL"
        "TestProcess_LowTTLValue"
        "TestProcess_HTTPRequestsHaveTimeToFinish"
        "TestProcess_SwapState"
        "TestProcess_ShutdownInterruptsHealthCheck"
        "TestProcess_ExitInterruptsHealthCheck"
        "TestProcess_ConcurrencyLimit"
        "TestProcess_StopImmediately"
        "TestProcess_ForceStopWithKill"
        "TestProcess_StopCmd"
        "TestProcess_EnvironmentSetCorrectly"
        "TestProcess_ReverseProxyPanicIsHandled"
      ];
    in
    lib.optionals (skippedTests != [ ]) [ "-skip=^${builtins.concatStringsSep "$|^" skippedTests}$" ];

  # Development and regression tools that have no place in the installed
  # output. simple-responder stays: the tests need it, and postCheck drops it.
  excludedPackages = [
    "cmd/fake-model"
    "cmd/monitor-test"
    "cmd/test-concurrency"
    "cmd/misc/benchmark-chatcompletion"
    "cmd/misc/process-cmd-test"
  ];

  # Tests execute the simple-responder helper from ./build and check the
  # file exists, so it cannot run under cross compilation.
  doCheck = canExecute;
  preCheck = ''
    mkdir build
    ln -s "$GOPATH/bin/simple-responder" "./build/simple-responder_''${GOOS}_''${GOARCH}"
  '';
  postCheck = ''
    rm "$GOPATH/bin/simple-responder"
  '';

  postInstall = ''
    install -Dm444 -t "$out/share/llama-swap" config.example.yaml
    mkdir -p "$wol/bin"
    mv "$out/bin/wol-proxy" "$wol/bin/"
  '';

  doInstallCheck = true;
  versionCheckProgramArg = "-version";

  __structuredAttrs = true;

  meta = {
    homepage = "https://github.com/Omegaice/llama-swap";
    description = "Model swapping for llama.cpp (or any local OpenAPI compatible server), with the fleet patches";
    license = lib.licenses.mit;
    mainProgram = "llama-swap";
    platforms = lib.platforms.unix;
  };
})
