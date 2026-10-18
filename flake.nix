{
  description = "SeaweedFS In-Cluster Admin Web UI — Executable, Container Image, and Helm Chart";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-24.11";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        lib = pkgs.lib;

        version = "0.1.0";
        gitCommit = self.shortRev or self.dirtyShortRev or "dev";

        # For container images built on macOS (darwin), target the corresponding Linux system
        linuxSystem =
          if system == "aarch64-darwin" then "aarch64-linux"
          else if system == "x86_64-darwin" then "x86_64-linux"
          else system;
        linuxPkgs = import nixpkgs { system = linuxSystem; };

        mkSeaweedfsUI = goPkgs: goPkgs.buildGoModule {
          pname = "seaweedfs-ui";
          inherit version;

          src = lib.cleanSourceWith {
            src = ./.;
            filter = path: type:
              let
                relPath = lib.removePrefix (toString ./. + "/") (toString path);
              in
              !(lib.hasPrefix ".agents" relPath) &&
              !(lib.hasPrefix ".git" relPath) &&
              !(lib.hasPrefix "result" relPath);
          };

          vendorHash = null;

          subPackages = [ "cmd/seaweedfs-ui" ];

          env = {
            CGO_ENABLED = "0";
          };

          ldflags = [
            "-s"
            "-w"
            "-X main.Version=${version}"
            "-X main.GitCommit=${gitCommit}"
            "-X main.BuildDate=1970-01-01T00:00:00Z"
          ];

          # Run unit tests that don't require external helm binary during Go module build
          doCheck = true;
          checkFlags = [ "-skip=TestHelmChartConfigurations" ];

          postInstall = ''
            mkdir -p $out/usr/local/bin
            ln -s $out/bin/seaweedfs-ui $out/usr/local/bin/seaweedfs-ui
          '';

          meta = with lib; {
            description = "SeaweedFS In-Cluster Admin Web UI";
            mainProgram = "seaweedfs-ui";
            platforms = platforms.unix;
          };
        };

        # 1. Derivation 1: The compiled seaweedfs-ui executable
        seaweedfs-ui = mkSeaweedfsUI pkgs;

        # Linux executable used inside the OCI/Docker image
        seaweedfs-ui-linux =
          if pkgs.stdenv.isLinux then seaweedfs-ui
          else (mkSeaweedfsUI linuxPkgs);

        # Runtime filesystem layout with non-root permissions and binary paths matching SPEC.md
        containerRoot = pkgs.runCommand "seaweedfs-ui-container-root" { } ''
          mkdir -p $out/usr/local/bin $out/usr/bin $out/etc/seaweedfs-ui $out/tmp/seaweedfs/.seaweedfs
          ln -s ${seaweedfs-ui-linux}/bin/seaweedfs-ui $out/usr/local/bin/seaweedfs-ui
          ln -s ${linuxPkgs.seaweedfs}/bin/weed $out/usr/local/bin/weed
          ln -s ${linuxPkgs.seaweedfs}/bin/weed $out/usr/bin/weed
        '';

        # 2. Derivation 2: The OCI/Docker container image (chained from seaweedfs-ui executable)
        docker-image = pkgs.dockerTools.buildLayeredImage {
          name = "ghcr.io/kevinpthorne/seaweedfs-ui";
          tag = version;
          created = "1970-01-01T00:00:01Z";
          architecture = linuxPkgs.go.GOARCH;

          contents = [
            linuxPkgs.cacert
            linuxPkgs.tzdata
            seaweedfs-ui-linux
            linuxPkgs.seaweedfs
            containerRoot
          ];

          extraCommands = ''
            mkdir -p tmp/seaweedfs/.seaweedfs etc/seaweedfs-ui
            chmod 1777 tmp tmp/seaweedfs tmp/seaweedfs/.seaweedfs
          '';

          config = {
            User = "65532:65532";
            Env = [
              "HOME=/tmp/seaweedfs"
              "WEED_BINARY_PATH=/usr/local/bin/weed"
              "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"
              "PATH=/usr/local/bin:/usr/bin:/bin"
            ];
            ExposedPorts = {
              "8080/tcp" = { };
            };
            Entrypoint = [ "/usr/local/bin/seaweedfs-ui" ];
            Cmd = [ "-config" "/etc/seaweedfs-ui/config.yaml" ];
            Labels = {
              "org.opencontainers.image.title" = "seaweedfs-ui";
              "org.opencontainers.image.version" = version;
              "org.opencontainers.image.revision" = gitCommit;
            };
          };

          passthru = {
            inherit version;
            executable = seaweedfs-ui;
          };
        };

        # 3. Derivation 3: The packaged Helm chart (chained from executable + docker-image)
        helm-chart = pkgs.stdenvNoCC.mkDerivation {
          pname = "seaweedfs-ui-helm-chart";
          inherit version;

          src = ./charts/seaweedfs-ui;

          nativeBuildInputs = [ pkgs.kubernetes-helm ];

          # Explicitly chain the executable and docker-image derivations so Nix builds
          # and verifies them before packaging the Helm chart.
          buildInputs = [ seaweedfs-ui docker-image ];

          dontConfigure = true;

          buildPhase = ''
            runHook preBuild

            echo "Verified upstream executable: ${seaweedfs-ui}/bin/seaweedfs-ui"
            echo "Verified upstream container image archive: ${docker-image}"

            mkdir -p build/seaweedfs-ui
            cp -R ./* build/seaweedfs-ui/
            chmod -R u+w build/seaweedfs-ui

            # Ensure Chart.yaml version & appVersion match the Nix derivation version
            substituteInPlace build/seaweedfs-ui/Chart.yaml \
              --replace-quiet 'version: 0.1.0' 'version: ${version}' \
              --replace-quiet 'appVersion: "0.1.0"' 'appVersion: "${version}"'

            runHook postBuild
          '';

          doCheck = true;
          checkPhase = ''
            runHook preCheck

            export HELM_CACHE_HOME="$TMPDIR/helm-cache"
            export HELM_CONFIG_HOME="$TMPDIR/helm-config"
            export HELM_DATA_HOME="$TMPDIR/helm-data"

            helm lint build/seaweedfs-ui
            helm template test-release build/seaweedfs-ui --namespace seaweedfs > /dev/null
            helm template test-release build/seaweedfs-ui --namespace seaweedfs \
              --set rbac.clusterWide=false \
              --set config.tls.enabled=true \
              --set config.tls.existingSecret=seaweedfs-client-tls \
              --set ingress.enabled=true > /dev/null

            runHook postCheck
          '';

          installPhase = ''
            runHook preInstall

            mkdir -p $out
            helm package build/seaweedfs-ui --destination $out --version ${version} --app-version ${version}

            cat > $out/release-metadata.json <<EOF
            {
              "version": "${version}",
              "gitCommit": "${gitCommit}",
              "executable": "${seaweedfs-ui}",
              "dockerImage": "${docker-image}",
              "chartArchive": "seaweedfs-ui-${version}.tgz"
            }
            EOF

            runHook postInstall
          '';
        };
      in
      {
        packages = {
          default = seaweedfs-ui;
          inherit seaweedfs-ui docker-image helm-chart;
        };

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            kubernetes-helm
            seaweedfs
            skopeo
          ];
        };
      }
    );
}
