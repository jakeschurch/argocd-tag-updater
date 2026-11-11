{
  description = "argocd-tag-updater — generic CR field updater driven by git/OCI tag patterns";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      inherit (pkgs) lib;

      # Go build inputs only. The source scope is the cache-buster: edits to
      # docs, charts, config or CI files must not change the bundle outPath
      # (the tag-updater deploys itself, so a spurious rebuild is a spurious
      # self-update). vendor/ is checked in, so the build needs nothing else.
      goFiles = lib.fileset.fileFilter
        (file: file.hasExt "go" && !(lib.hasSuffix "_test.go" file.name));
      buildSrc = lib.fileset.toSource {
        root = ./.;
        fileset = lib.fileset.unions [
          ./go.mod
          ./go.sum
          ./vendor
          (goFiles ./api)
          (goFiles ./cmd)
          (goFiles ./internal)
        ];
      };
      # The verification derivation additionally needs the tests.
      testSrc = lib.fileset.toSource {
        root = ./.;
        fileset = lib.fileset.unions [
          ./go.mod
          ./go.sum
          ./vendor
          ./config/crd
          (lib.fileset.fileFilter (file: file.hasExt "go") ./api)
          (lib.fileset.fileFilter (file: file.hasExt "go") ./cmd)
          (lib.fileset.fileFilter (file: file.hasExt "go") ./internal)
        ];
      };

      # Vet and tests, recorded as data. This derivation never fails the build:
      # go.json and vet.log are preserved either way and the consumer (the
      # nix-go-test check template) enforces exit-code. Live registry tests
      # self-skip unless explicitly enabled. Tests shell out to git, so git is
      # a native build input.
      testReport = pkgs.buildGoModule {
        pname = "argocd-tag-updater-go-verification";
        version = "0.1.0";
        src = testSrc;
        vendorHash = null;
        env.CGO_ENABLED = "0";
        nativeBuildInputs = [ pkgs.git ];
        # Non-sandboxed builds (the homelab-pr-checks pod) refuse to start a
        # derivation while /homeless-shelter exists; keep Go's HOME in TMPDIR.
        preBuild = "export HOME=$TMPDIR";
        doCheck = false;
        buildPhase = ''
          runHook preBuild
          mkdir verification
          status=0
          if go vet ./... > verification/vet.log 2>&1; then
            echo 0 > verification/vet-exit-code
          else
            echo "$?" > verification/vet-exit-code
            status=1
          fi
          if go test -json ./... > verification/go.json; then
            echo 0 > verification/test-exit-code
          else
            echo "$?" > verification/test-exit-code
            status=1
          fi
          printf '%s\n' "$status" > verification/exit-code
          runHook postBuild
        '';
        installPhase = ''
          cp -R verification "$out"
        '';
      };
    in
    {
      packages.${system} = rec {
        # Vet and tests as data (go.json, vet.log, exit-code). The ci/margo
        # nix-go-test check builds this attribute directly.
        inherit testReport;

        argocd-tag-updater = pkgs.buildGoModule {
          pname = "argocd-tag-updater";
          version = "0.1.0";
          src = buildSrc;
          subPackages = [ "cmd" ];
          vendorHash = null; # fill in after first `nix build`
          env.CGO_ENABLED = "0";
          ldflags = [ "-s" "-w" "-X main.version=0.1.0" ];
          doCheck = false;
          passthru = { inherit testReport; };
          meta.mainProgram = "argocd-tag-updater";
          postInstall = ''
            mv $out/bin/cmd $out/bin/argocd-tag-updater
          '';
        };

        # NixMount target — statically-linked binary plus a CA bundle.
        # Git operations use go-git in-process (see internal/source/git.go),
        # so no external git binary is needed at runtime. The pod base
        # image can be scratch; SSL_CERT_FILE points into this mount.
        #
        # Must be a self-contained tree of real files, not a symlinkJoin.
        # nix-csi-go bind-mounts only this single store path into the pod,
        # so symlinks pointing back to other /nix/store paths would dangle
        # inside a scratch container.
        argocd-tag-updater-bundle = pkgs.runCommand "argocd-tag-updater-bundle" { } ''
          mkdir -p $out/bin $out/etc/ssl/certs
          cp ${argocd-tag-updater}/bin/argocd-tag-updater $out/bin/argocd-tag-updater
          chmod +x $out/bin/argocd-tag-updater
          cp ${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt $out/etc/ssl/certs/ca-bundle.crt
        '';

        argocd-tag-updater-image = pkgs.dockerTools.buildLayeredImage {
          name = "argocd-tag-updater";
          tag = "latest";
          contents = [ pkgs.cacert pkgs.dockerTools.caCertificates pkgs.git ];
          extraCommands = ''
            mkdir -p bin
            cp ${argocd-tag-updater}/bin/argocd-tag-updater bin/argocd-tag-updater
            chmod +x bin/argocd-tag-updater
          '';
          config = {
            Entrypoint = [ "/bin/argocd-tag-updater" ];
            ExposedPorts = {
              "8080/tcp" = { }; # metrics
              "8081/tcp" = { }; # health probes
            };
            Env = [ "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt" ];
          };
        };

        default = argocd-tag-updater-image;
      };

      checks.${system} = {
        # testReport always builds; this is the check that fails when vet or a
        # test did.
        test-report = pkgs.runCommand "argocd-tag-updater-test-report-check" { } ''
          test -s ${testReport}/go.json
          test "$(cat ${testReport}/exit-code)" = 0
          touch $out
        '';

        # The build source holds Go build inputs only: no docs, charts, config,
        # flake or test files outside vendor/, so non-Go edits keep the bundle
        # outPath.
        build-src-scope = pkgs.runCommand "argocd-tag-updater-src-scope" { } ''
          cd ${buildSrc}
          unexpected=$(find . -type f ! -path './vendor/*' \
            ! -name go.mod ! -name go.sum \
            \( ! -name '*.go' -o -name '*_test.go' \) | sort)
          if [ -n "$unexpected" ]; then
            echo "build source contains non-build inputs:" >&2
            echo "$unexpected" >&2
            exit 1
          fi
          touch $out
        '';
      };
    };
}
