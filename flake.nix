{
  description = "argocd-tag-updater — generic CR field updater driven by git/OCI tag patterns";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
    in
    {
      packages.${system} = rec {
        argocd-tag-updater = pkgs.buildGoModule {
          pname = "argocd-tag-updater";
          version = "0.1.0";
          src = ./.;
          subPackages = [ "cmd" ];
          vendorHash = null; # fill in after first `nix build`
          env.CGO_ENABLED = "0";
          ldflags = [ "-s" "-w" "-X main.version=0.1.0" ];
          doCheck = false;
          meta.mainProgram = "argocd-tag-updater";
        };

        argocd-tag-updater-image = pkgs.dockerTools.buildLayeredImage {
          name = "argocd-tag-updater";
          tag = "latest";
          contents = [ pkgs.cacert pkgs.dockerTools.caCertificates pkgs.git ];
          extraCommands = ''
            mkdir -p bin
            cp ${argocd-tag-updater}/bin/cmd bin/argocd-tag-updater
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
    };
}
