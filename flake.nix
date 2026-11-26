{
  description = "Argo CD package, repository checks and development shell";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs?rev=674a2028661e550a97400c1cc992b9cca9a668a1";

  outputs =
    { self, nixpkgs }:
    let
      forLinux = nixpkgs.lib.genAttrs [
        "x86_64-linux"
        "aarch64-linux"
      ];
      perSystem = forLinux (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          arch = if system == "x86_64-linux" then "amd64" else "arm64";
          protocArch = if system == "x86_64-linux" then "x86_64" else "aarch_64";
          checksum =
            file:
            builtins.head (
              pkgs.lib.splitString " " (builtins.readFile (./hack/installers/checksums + "/${file}.sha256"))
            );

          # Use the same releases and checksums as hack/installers.
          protocArchive = "protoc-29.3-linux-${protocArch}.zip";
          protoc = pkgs.stdenv.mkDerivation {
            pname = "argocd-protoc";
            version = "29.3";
            src = pkgs.fetchurl {
              url = "https://github.com/protocolbuffers/protobuf/releases/download/v29.3/${protocArchive}";
              sha256 = checksum protocArchive;
            };
            nativeBuildInputs = [
              pkgs.unzip
              pkgs.autoPatchelfHook
            ];
            buildInputs = [ pkgs.stdenv.cc.cc.lib ];
            sourceRoot = ".";
            installPhase = "mkdir -p $out && cp -r bin include $out/";
          };
          kustomize =
            pkgs.runCommand "argocd-kustomize-5.8.3"
              {
                src = pkgs.fetchurl {
                  url = "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize/v5.8.3/kustomize_v5.8.3_linux_${arch}.tar.gz";
                  sha256 = checksum "kustomize_5.8.3_linux_${arch}.tar.gz";
                };
              }
              ''
                mkdir -p "$out/bin"
                tar -xzf "$src" -C "$out/bin" kustomize
              '';

          # pnpm 12 ships a native executable. Patch its ELF interpreter for NixOS.
          pnpmArch = if system == "x86_64-linux" then "x64" else "arm64";
          pnpm = pkgs.stdenv.mkDerivation {
            passthru = { inherit (pkgs.pnpm) nodejs-slim; };
            pname = "pnpm";
            version = "12.3.4";
            src = pkgs.fetchurl {
              url = "https://registry.npmjs.org/@pnpm/exe.linux-${pnpmArch}/-/exe.linux-${pnpmArch}-12.3.4.tgz";
              hash =
                {
                  x86_64-linux = "sha512-2ZqOlSPkfwX1h5cR+FPiWf8+F+2hZT/3TvhUK5sigHqwaQCIiq8R7CGxhndKs63JtcLi2a1Qpo+wX/EoyfjyJQ==";
                  aarch64-linux = "sha512-t71AVA7LRqiKTyZ5xMYaZc2n5DfdpMbfokZuiIOXHBOM03ECnF0t4iYwaBDqJgVjlKYUOwaF/bRQajGNA4cJ4w==";
                }
                .${system};
            };
            payload = pkgs.fetchurl {
              url = "https://registry.npmjs.org/pnpm/-/pnpm-12.3.4.tgz";
              hash = "sha512-lhqkH7B32joEpEHZ+OFevAyW2o73ELLrZ7+e58sGEOq9SPH9hfUc/+c4RnhfoPh8VqOocqHYk/hEZ0G1zORUVw==";
            };
            nativeBuildInputs = [ pkgs.autoPatchelfHook ];
            buildInputs = [ pkgs.stdenv.cc.cc.lib ];
            installPhase = ''
              mkdir -p "$out/lib/pnpm" "$out/bin"
              install -m755 pnpm "$out/lib/pnpm/"
              tar -xzf "$payload" -C "$out/lib/pnpm" --strip-components=1 package/dist
              ln -s "$out/lib/pnpm/pnpm" "$out/bin/pnpm"
            '';
          };

          # Run once at the checkout root before codegen-local and lint-local.
          # Reuse the repo's Go module versions and its mockery build workaround.
          prepare = pkgs.writeShellScriptBin "argocd-prepare-tools" ''
            set -eu
            cd "$(git rev-parse --show-toplevel)"
            export GOBIN="$PWD/dist"
            mkdir -p "$GOBIN/protoc-include"
            cp -r --no-preserve=mode ${protoc}/include/. "$GOBIN/protoc-include/"
            bash hack/installers/install-codegen-go-tools.sh
            bash hack/installers/install-lint-tools.sh
          '';
          # Some upstream scripts invoke /bin/bash and /bin/rm by absolute path.
          # Supply /bin on NixOS without changing the host or requiring a container daemon.
          scriptBin = pkgs.buildEnv {
            name = "argocd-script-bin";
            paths = [
              pkgs.bash
              pkgs.coreutils
            ];
            pathsToLink = [ "/bin" ];
          };
          nativeMake = pkgs.writeShellScriptBin "argocd-make" ''
            if [[ -x /bin/bash && -x /bin/rm ]]; then
              exec ${pkgs.gnumake}/bin/make "$@"
            fi
            exec ${pkgs.proot}/bin/proot -b ${scriptBin}/bin:/bin ${pkgs.gnumake}/bin/make "$@"
          '';
          localChecks = pkgs.writeShellScriptBin "argocd-check" ''
            export PATH=${
              pkgs.lib.makeBinPath [
                (toolsFor "codegen-go")
                (toolsFor "lint")
              ]
            }:"$PATH"
            prepareCodegen=${prepareCodegen}
            source ${./hack/check-local.sh}
          '';
          tools = with pkgs; [
            bashInteractive
            coreutils
            findutils
            gawk
            gnugrep
            gnused
            diffutils
            gnumake
            which
            git
            git-lfs
            openssh
            gnupg
            cacert
            curl
            jq
            gnutar
            gzip
            unzip
            zip
            go_1_27
            stdenv.cc
            gotestsum
            go-junit-report
            nodejs_24
            pnpm
            python3
            kubernetes-helm
            kustomize
            kubectl
            redis
            oras
            distribution
            protoc
            prepare
            nativeMake
            localChecks
          ];
          version = pkgs.lib.removeSuffix "\n" (builtins.readFile ./VERSION);
          src = pkgs.lib.fileset.toSource {
            root = ./.;
            fileset = pkgs.lib.fileset.difference ./. (
              pkgs.lib.fileset.unions [
                ./flake.nix
                ./flake.lock
              ]
            );
          };
          toolSource = pkgs.lib.fileset.toSource {
            root = ./.;
            fileset = pkgs.lib.fileset.unions [
              ./go.mod
              ./go.sum
              ./gitops-engine/go.mod
              ./gitops-engine/go.sum
              ./hack/installers/install-codegen-go-tools.sh
              ./hack/installers/install-lint-tools.sh
            ];
          };
          goProxy =
            name: source: hash: resolve:
            (pkgs.buildGo127Module {
              pname = name;
              inherit version;
              src = source;
              proxyVendor = true;
              vendorHash = hash;
              modPostBuild = resolve;
            }).goModules;

          modules = goProxy "argocd" src "sha256-fOVb3yQZVqr62j9oNOv7Vv10SjVjL/jyGXqTQagEfAE=" ''
            go mod tidy
            (cd gitops-engine && go mod download && go mod tidy)
          '';
          # -n resolves the installer's actual packages, without compiling executables
          # into a fixed-output derivation. Both Linux targets share this module proxy.
          toolModules =
            goProxy "argocd-check-tools" toolSource "sha256-JiOxYMmnhTDIPcx6v0jgGiqwBiEgPBvbYtrRsXAvozo="
              ''
                rmdir vendor
                go() {
                  if [[ "$1" == install ]]; then
                    shift
                    GOBIN= command go install -n "$@" 2>"$TMPDIR/install-plan" || { tail -50 "$TMPDIR/install-plan" >&2; return 1; }
                  else
                    command go "$@"
                  fi
                }
                export -f go
                export GOSUMDB=off
                for GOARCH in amd64 arm64; do
                  export GOARCH
                  bash hack/installers/install-codegen-go-tools.sh
                  bash hack/installers/install-lint-tools.sh
                done
                # go install pkg@ver and go get query the latest upstream release for
                # retractions, leaving floating un-zipped <latest>.{info,mod} files and
                # corresponding @v/list entries in the proxy cache. Strip them and
                # rebuild @v/list from the remaining pinned .mod files so this FOD stays
                # reproducible across upstream releases.
                shopt -s nullglob
                while IFS= read -r -d "" info; do
                  base="''${info%.info}"
                  if [[ ! -f "$base.zip" ]]; then
                    rm -f "$base.info" "$base.mod"
                    dir="$(dirname "$info")"
                    mods=("$dir"/*.mod)
                    if (( ''${#mods[@]} > 0 )); then
                      printf '%s\n' "''${mods[@]##*/}" | sed 's/\.mod$//' | LC_ALL=C sort -u > "$dir/list"
                    else
                      rm -f "$dir/list"
                    fi
                  fi
                done < <(find "$GOPATH/pkg/mod/cache/download" -path '*/@v/*.info' -print0)
              '';

          pnpmDeps = pkgs.fetchPnpmDeps {
            pname = "argocd-ui";
            src = ./ui;
            inherit pnpm;
            fetcherVersion = 5;
            hash = "sha256-G/D2qecKX9jLzmy7yW20OEEreiX5tCQzdqlYxjEr5sk=";
          };
          uiTask =
            name: buildPhase: installPhase:
            pkgs.stdenvNoCC.mkDerivation {
              pname = "argocd-ui-${name}";
              inherit
                version
                pnpmDeps
                buildPhase
                installPhase
                ;
              nativeBuildInputs = [
                pkgs.nodejs_24
                pnpm
                pkgs.pnpmConfigHook
              ];
              src = ./ui;
              CI = "true";
            };
          success = ''touch "$out"'';
          ui = uiTask "build" ''
            NODE_ENV=production NODE_ONLINE_ENV=online NODE_OPTIONS=--max-old-space-size=8192 \
              ARGO_VERSION=${version} pnpm build
            test -s dist/app/index.html
          '' ''cp -r dist/app "$out"'';

          # The sandbox has no .git directory. Some generators and tests require a
          # checkout, and the lint/codegen checks need a baseline for their diff.
          checkout = ''
            git config --global user.name 'Argo CD checks'
            git config --global user.email 'checks@localhost'
            git add -f .
            git commit -qm 'Source snapshot'
          '';
          goTask =
            {
              name,
              buildPhase,
              inputs ? [ ],
              installPhase ? success,
              proxy ? modules,
              source ? src,
              meta ? { },
            }:
            pkgs.stdenv.mkDerivation {
              pname = "argocd-${name}";
              inherit
                version
                buildPhase
                installPhase
                ;
              nativeBuildInputs = [
                pkgs.go_1_27
                pkgs.git
                pkgs.which
                pkgs.gnumake
                pkgs.util-linux
              ]
              ++ inputs;
              src = source;
              GOSUMDB = "off";
              GOPROXY = "file://${proxy}";
              GOTOOLCHAIN = "local";
              GOFLAGS = "-trimpath -buildvcs=false";
              GRPC_ENABLE_TXT_SERVICE_CONFIG = "false";
              GIT_COMMIT = self.rev or (pkgs.lib.removeSuffix "-dirty" (self.dirtyRev or "unknown"));
              GIT_TREE_STATE =
                if self ? dirtyRev then
                  "dirty"
                else if self ? rev then
                  "clean"
                else
                  "unknown";
              configurePhase = ''
                export HOME="$TMPDIR/home" GOPATH="$TMPDIR/go" GOCACHE="$TMPDIR/go-cache"
                export GOMAXPROCS="$NIX_BUILD_CORES"
                export BUILD_DATE="$(date -u -d "@$SOURCE_DATE_EPOCH" +%FT%TZ)"
                mkdir -p "$HOME" "$GOPATH" dist
                git init -q -b master
              '';
              inherit meta;
            };

          toolsFor =
            name:
            (goTask {
              name = "${name}-tools";
              proxy = toolModules;
              source = toolSource;
              buildPhase = ''
                export GOBIN="$PWD/dist"
                bash hack/installers/install-${name}-tools.sh
              '';
              installPhase = ''mkdir -p "$out" && mv dist "$out/bin"'';
            }).overrideAttrs
              {
                # Installer binaries depend on the tool sources, independent of the checkout revision.
                GIT_COMMIT = "";
                GIT_TREE_STATE = "";
              };
          redisChart = pkgs.fetchurl {
            url = "https://github.com/DandyDeveloper/charts/releases/download/redis-ha-4.35.10/redis-ha-4.35.10.tgz";
            sha256 = "d6fd5a047bfab24ac91148b48ade18c49947a2da5d0f9adc87c24e6027456573";
          };
          # Both codegen checks run this in a disposable copy of the checkout.
          prepareCodegen = pkgs.writeShellScript "prepare-codegen" ''
            set -eu
            mkdir -p dist
            ln -s ${protoc}/include dist/protoc-include
            cd manifests/ha/base/redis-ha
            grep -Fx '  version: 4.35.10' chart/requirements.yaml
            grep -Fx 'helm dependency update ./chart' generate.sh
            sed -i 's|^helm dependency update ./chart$|mkdir -p chart/charts \&\& cp ${redisChart} chart/charts/redis-ha-4.35.10.tgz|' generate.sh
          '';
          examples = pkgs.fetchgit {
            url = "https://github.com/argoproj/argocd-example-apps.git";
            rev = "8088f4c0d970abb09e250248cc97e35623447cb5";
            deepClone = true;
            fetchSubmodules = false;
            branchName = "master";
            hash = "sha256-ZNORqBT/iYJKwJrFf3jLbR6XNiOdVX1Ff/WMf5hcU6s=";
            postFetch = ''
              git -C "$out" -c pack.threads=1 bundle create "$TMPDIR/examples.bundle" refs/heads/master HEAD
              cp "$out/helm-guestbook/values.yaml" "$TMPDIR/values.yaml"
              rm -rf "$out"
              mkdir -p "$out"
              mv "$TMPDIR/examples.bundle" "$TMPDIR/values.yaml" "$out/"
            '';
          };
          helmFixture =
            file: sha256:
            pkgs.fetchurl {
              url = "https://raw.githubusercontent.com/argoproj/argo-helm/877f58ee43b7f3ce99a7f1cf216bebb1a28e1b54/${file}";
              inherit sha256;
            };
          fixtures = pkgs.linkFarm "argocd-test-fixtures" {
            "examples.bundle" = "${examples}/examples.bundle";
            "guestbook/values.yaml" = "${examples}/values.yaml";
            "argo-helm/index.yaml" =
              helmFixture "index.yaml" "69699a747a64ce85d9c1269b0efc4d4eb81d4b669719b67d7be0099e463afb33";
            "argo-helm/argo-cd-0.7.1.tgz" =
              helmFixture "argo-cd-0.7.1.tgz" "f1fb9caffcdd01ac0055743499434d443808b15c694bde3dcab430c088ca5615";
          };
          argocd = goTask {
            name = "package";
            meta.mainProgram = "argocd";
            buildPhase = ''
              cp -r ${ui}/. ui/dist/app/
              make GIT_TAG= argocd-all
            '';
            installPhase = ''
              install -Dm755 dist/argocd "$out/bin/argocd"
              "$out/bin/argocd" version --client
              for cmd in server repo-server cmp-server commit-server dex notifications \
                application-controller applicationset-controller git-ask-pass k8s-auth; do
                ln -s argocd "$out/bin/argocd-$cmd"
              done
            '';
          };
        in
        {
          packages.default = argocd;
          devShells.default = pkgs.mkShell {
            packages = tools;
            shellHook = ''
              root="$(git rev-parse --show-toplevel 2>/dev/null || pwd -P)"
              export PATH="$root/dist:$root/hack:$PATH"
              export ARGOCD_TEST_FIXTURES=${fixtures}
              if [[ -f "$root/hack/installers/install-codegen-go-tools.sh" ]]; then
                mkdir -p "$root/.scratch"
                if [[ -L "$root/.scratch/nix-fixtures" || ! -e "$root/.scratch/nix-fixtures" ]]; then
                  ln -sfnT "$ARGOCD_TEST_FIXTURES" "$root/.scratch/nix-fixtures"
                  export ARGOCD_TEST_FIXTURES="$root/.scratch/nix-fixtures"
                fi
              fi
              export GRPC_ENABLE_TXT_SERVICE_CONFIG=false
            '';
          };
          checks = {
            package = argocd;
            build = goTask {
              name = "build";
              buildPhase = "make GIT_TAG= build-local";
            };
            cli = goTask {
              name = "cli";
              buildPhase = ''
                make GIT_TAG= cli-local
                dist/argocd version --client
              '';
            };
            lint = goTask {
              name = "lint";
              inputs = [ (toolsFor "lint") ];
              buildPhase = checkout + ''
                make lint-local
                git diff --exit-code
              '';
            };
            codegen = goTask {
              name = "codegen";
              inputs = [
                (toolsFor "codegen-go")
                protoc
                kustomize
                pkgs.kubernetes-helm
                pkgs.jq
                pkgs.which
              ];
              buildPhase = ''
                patchShebangs hack manifests/ha/base/redis-ha/generate.sh
                substituteInPlace hack/generate-proto.sh --replace-fail '/bin/rm' 'rm'
                ${prepareCodegen}
              ''
              + checkout
              + ''
                IMAGE_TAG="v${version}" make codegen-local
                git diff --exit-code -- . ':!go.sum' ':!go.mod' ':!assets/swagger.json'
              '';
            };
            test = goTask {
              name = "test";
              inputs = [
                nativeMake
                pkgs.gotestsum
                pkgs.go-junit-report
                pkgs.openssh
                pkgs.git-lfs
                pkgs.gnupg
                pkgs.kubernetes-helm
                kustomize
                pkgs.kubectl
                pkgs.redis
                pkgs.oras
                pkgs.distribution
              ];
              buildPhase = checkout + ''
                export ARGOCD_TEST_FIXTURES=${fixtures} CI=true
                export PATH="$PWD/hack:$PATH"
                argocd-make test-local
              '';
            };
            # make lint-ui-local runs the UI package's lint script.
            lint-ui = uiTask "lint" "pnpm lint" success;
            test-ui = uiTask "test" "NODE_ENV=test pnpm test --runInBand" success;
          };
        }
      );
    in
    {
      packages = builtins.mapAttrs (_: value: value.packages) perSystem;
      devShells = builtins.mapAttrs (_: value: value.devShells) perSystem;
      checks = builtins.mapAttrs (_: value: value.checks) perSystem;
    };
}
