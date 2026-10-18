{
  description = "SeaweedFS In-Cluster Admin Web UI — Multi-Arch Executable, Container Images (amd64/arm64), and Helm Chart";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        lib = pkgs.lib;

        version = "0.1.0";
        gitCommit = self.shortRev or self.dirtyShortRev or "dev";

        cleanedSrc = lib.cleanSourceWith {
          src = ./.;
          filter = path: type:
            let
              relPath = lib.removePrefix (toString ./. + "/") (toString path);
            in
            !(lib.hasPrefix ".agents" relPath) &&
            !(lib.hasPrefix ".git" relPath) &&
            !(lib.hasPrefix "result" relPath);
        };

        commonLdflags = [
          "-s"
          "-w"
          "-X main.Version=${version}"
          "-X main.GitCommit=${gitCommit}"
          "-X main.BuildDate=1970-01-01T00:00:00Z"
        ];

        # 1a. Native executable (runs unit test suite during checkPhase)
        seaweedfs-ui = pkgs.buildGoModule {
          pname = "seaweedfs-ui";
          inherit version;
          src = cleanedSrc;
          vendorHash = null;
          subPackages = [ "cmd/seaweedfs-ui" ];
          env = {
            CGO_ENABLED = 0;
          };
          ldflags = commonLdflags;
          doCheck = true;

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

        # 1b. Cross/target Linux static Go executable builder for amd64 & arm64
        mkLinuxExecutable = goArch: pkgs.buildGoModule {
          pname = "seaweedfs-ui-linux-${goArch}";
          inherit version;
          src = cleanedSrc;
          vendorHash = null;
          subPackages = [ "cmd/seaweedfs-ui" ];
          env = {
            CGO_ENABLED = 0;
            GOOS = "linux";
            GOARCH = goArch;
          };
          ldflags = commonLdflags;
          # Unit tests already run in the native `seaweedfs-ui` derivation
          doCheck = false;

          postInstall = ''
            # When cross-compiling, `go install` outputs to $out/bin/linux_${goArch}/seaweedfs-ui
            if [ -f "$out/bin/linux_${goArch}/seaweedfs-ui" ]; then
              mv "$out/bin/linux_${goArch}/seaweedfs-ui" "$out/bin/seaweedfs-ui"
              rmdir "$out/bin/linux_${goArch}"
            fi
            mkdir -p $out/usr/local/bin
            ln -s $out/bin/seaweedfs-ui $out/usr/local/bin/seaweedfs-ui
          '';
        };

        seaweedfs-ui-linux-amd64 = mkLinuxExecutable "amd64";
        seaweedfs-ui-linux-arm64 = mkLinuxExecutable "arm64";

        # 2. Multi-arch OCI/Docker layered image builder (amd64 and arm64)
        mkDockerImage = { goArch, nixSystem, appBin }:
          let
            targetLinuxPkgs = import nixpkgs { system = nixSystem; };
            containerRoot = pkgs.runCommand "seaweedfs-ui-container-root-${goArch}" { } ''
              mkdir -p $out/usr/local/bin $out/usr/bin $out/etc/seaweedfs-ui $out/tmp/seaweedfs/.seaweedfs
              ln -s ${appBin}/bin/seaweedfs-ui $out/usr/local/bin/seaweedfs-ui
              ln -s ${targetLinuxPkgs.seaweedfs}/bin/weed $out/usr/local/bin/weed
              ln -s ${targetLinuxPkgs.seaweedfs}/bin/weed $out/usr/bin/weed
            '';
          in
          pkgs.dockerTools.buildLayeredImage {
            name = "ghcr.io/seaweedfs-ui/seaweedfs-ui";
            tag = "${version}-${goArch}";
            created = "1970-01-01T00:00:01Z";
            architecture = goArch;

            contents = [
              targetLinuxPkgs.cacert
              targetLinuxPkgs.tzdata
              appBin
              targetLinuxPkgs.seaweedfs
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
                "org.opencontainers.image.architecture" = goArch;
              };
            };

            passthru = {
              inherit version goArch;
              executable = appBin;
            };
          };

        docker-image-amd64 = mkDockerImage {
          goArch = "amd64";
          nixSystem = "x86_64-linux";
          appBin = seaweedfs-ui-linux-amd64;
        };

        docker-image-arm64 = mkDockerImage {
          goArch = "arm64";
          nixSystem = "aarch64-linux";
          appBin = seaweedfs-ui-linux-arm64;
        };

        docker-image =
          if system == "aarch64-linux" || system == "aarch64-darwin"
          then docker-image-arm64
          else docker-image-amd64;

        # 3. Derivation 3: The packaged Helm chart
        # Note: Do not place `.tar.gz` image archives in `buildInputs` because `stdenv/setup`
        # attempts to `source` regular files found in `buildInputs` as setup hooks.
        # Referencing `${seaweedfs-ui}`, `${docker-image-amd64}`, and `${docker-image-arm64}`
        # directly in the build script and environment variables creates the exact Nix
        # derivation dependency chain (`seaweedfs-ui` -> `docker-image-*` -> `helm-chart`).
        helm-chart = pkgs.stdenvNoCC.mkDerivation {
          pname = "seaweedfs-ui-helm-chart";
          inherit version;

          src = ./charts/seaweedfs-ui;

          nativeBuildInputs = [ pkgs.kubernetes-helm ];

          dontConfigure = true;

          buildPhase = ''
            runHook preBuild

            test -x "${seaweedfs-ui}/bin/seaweedfs-ui"
            echo "Verified upstream executable: ${seaweedfs-ui}/bin/seaweedfs-ui"
            ${lib.optionalString pkgs.stdenv.isLinux ''
              test -s "${docker-image-amd64}"
              test -s "${docker-image-arm64}"
              echo "Verified upstream linux/amd64 image archive: ${docker-image-amd64}"
              echo "Verified upstream linux/arm64 image archive: ${docker-image-arm64}"
            ''}

            CHART_BUILD_DIR="$TMPDIR/chart-build/seaweedfs-ui"
            mkdir -p "$CHART_BUILD_DIR"
            cp -R . "$CHART_BUILD_DIR/"
            chmod -R u+w "$CHART_BUILD_DIR"

            # Ensure Chart.yaml version & appVersion match the Nix derivation version
            substituteInPlace "$CHART_BUILD_DIR/Chart.yaml" \
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

            CHART_BUILD_DIR="$TMPDIR/chart-build/seaweedfs-ui"
            helm lint "$CHART_BUILD_DIR"
            helm template test-release "$CHART_BUILD_DIR" --namespace seaweedfs > /dev/null
            helm template test-release "$CHART_BUILD_DIR" --namespace seaweedfs \
              --set rbac.clusterWide=false \
              --set config.tls.enabled=true \
              --set config.tls.existingSecret=seaweedfs-client-tls \
              --set ingress.enabled=true > /dev/null

            runHook postCheck
          '';

          installPhase = ''
            runHook preInstall

            CHART_BUILD_DIR="$TMPDIR/chart-build/seaweedfs-ui"
            mkdir -p "$out"
            helm package "$CHART_BUILD_DIR" --destination "$out" --version "${version}" --app-version "${version}"

            cat > "$out/release-metadata.json" <<EOF
            {
              "version": "${version}",
              "gitCommit": "${gitCommit}",
              "executable": "${seaweedfs-ui}",
              "dockerImageAmd64": "${if pkgs.stdenv.isLinux then "${docker-image-amd64}" else "linux-only"}",
              "dockerImageArm64": "${if pkgs.stdenv.isLinux then "${docker-image-arm64}" else "linux-only"}",
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
          helm = pkgs.kubernetes-helm;
          skopeo = pkgs.skopeo;
          inherit
            seaweedfs-ui
            seaweedfs-ui-linux-amd64
            seaweedfs-ui-linux-arm64
            docker-image
            docker-image-amd64
            docker-image-arm64
            helm-chart;
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
