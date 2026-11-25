# shellcheck shell=bash
# Sourced by the argocd-check command in the Nix development shell.
set -eu -o pipefail

checks=(build cli lint test lint-ui test-ui package codegen)
usage() {
    echo "Usage: argocd-check [$(IFS='|'; echo "${checks[*]}")] ..."
    echo "With no arguments, run every check. Exit nonzero if any check fails."
}

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
    usage
    exit 0
fi
if (( $# == 0 )); then
    set -- "${checks[@]}"
fi
for check in "$@"; do
    case "$check" in
        build|cli|lint|test|lint-ui|test-ui|package|codegen) ;;
        *) usage >&2; exit 2 ;;
    esac
done

root=$(git rev-parse --show-toplevel)
cd "$root"
export CI=true
version=$(cat VERSION)

check_codegen() (
    # Generators remove vendor and overwrite generated files. Compare against the
    # current working files, including uncommitted inputs, in a temporary checkout.
    tmp=$(mktemp -d)
    trap 'rm -rf -- "$tmp"' EXIT
    mkdir "$tmp/src"
    git ls-files --cached --others --exclude-standard -z |
        while IFS= read -r -d '' file; do
            if [[ -f $file || -L $file ]]; then
                printf '%s\0' "$file"
            fi
        done | tar -cf - --null -T - | tar -C "$tmp/src" -xf -
    cd "$tmp/src"
    "${prepareCodegen:?missing codegen setup from the Nix development shell}"
    git init -q -b master
    git add -f .
    git -c user.name=check -c user.email=check@localhost -c commit.gpgSign=false \
        -c core.hooksPath=/dev/null commit -qm 'Before code generation'

    # generate-proto.sh modifies GOPATH/src and looks up dependencies in GOPATH/pkg/mod.
    local modCache
    modCache=$(go env GOMODCACHE)
    export GOPATH="$tmp/go" GOMODCACHE="$modCache"
    mkdir -p "$GOPATH/pkg" "$modCache"
    ln -s "$modCache" "$GOPATH/pkg/mod"
    IMAGE_TAG="v$version" argocd-make codegen-local
    git add -N .
    git diff --exit-code HEAD -- . ':!go.sum' ':!go.mod' ':!assets/swagger.json'
)

run_check() {
    case "$1" in
        build) make build-local ;;
        cli)
            make cli-local
            dist/argocd version --client ;;
        lint) golangci-lint run --verbose ;;
        test) GIT_CONFIG_GLOBAL="$root/util/git/testdata/gitconfig" argocd-make test-local ;;
        lint-ui) make lint-ui-local ;;
        test-ui) NODE_ENV='test' pnpm --dir ui test --runInBand ;;
        package)
            NODE_ENV=production NODE_ONLINE_ENV=online NODE_OPTIONS=--max-old-space-size=8192 \
                ARGO_VERSION="$version" pnpm --dir ui build
            test -s ui/dist/app/index.html
            make argocd-all
            dist/argocd version --client ;;
        codegen) check_codegen ;;
    esac
}

failed=()
ui_setup=pending
for check in "$@"; do
    echo "==> $check"
    case "$check" in
        lint-ui|test-ui|package)
            if [[ $ui_setup == pending ]]; then
                if pnpm --dir ui install --frozen-lockfile; then
                    ui_setup=ready
                else
                    ui_setup=failed
                fi
            fi
            if [[ $ui_setup == failed ]]; then
                echo "Skipping $check because UI dependency installation failed." >&2
                failed+=("$check")
                continue
            fi ;;
    esac
    # Keep going after a failed check, while stopping that check at its first failed step.
    set +e
    (set -e; run_check "$check")
    status=$?
    set -e
    if (( status != 0 )); then
        failed+=("$check")
    fi
done

if (( ${#failed[@]} )); then
    printf 'Failed checks: %s\n' "${failed[*]}" >&2
    exit 1
fi
echo "All requested checks passed."
