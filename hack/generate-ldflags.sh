#!/usr/bin/env sh
# BUILD_TAG may be supplied by CI, for example BUILD_TAG=v0.0.1 make build.
set -eu

BUILD_TAG=${BUILD_TAG:-v0.0.1}
# Keep every -X value a single linker argument, even when supplied by CI.
case "$BUILD_TAG" in
    *[!A-Za-z0-9._+-]*)
        printf 'invalid BUILD_TAG: use only letters, digits, dots, underscores, + and -\n' >&2
        exit 1
        ;;
esac

ROOT=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
GitCommit=unknown
GitTreeState=unknown
if git -C "$ROOT" rev-parse --verify HEAD >/dev/null 2>&1; then
    GitCommit=$(git -C "$ROOT" rev-parse HEAD)
    if [ -n "$(git -C "$ROOT" status --porcelain --untracked-files=normal)" ]; then
        GitTreeState=dirty
    else
        GitTreeState=clean
    fi
fi
BuildDate=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
PACKAGE=github.com/DR1N0/lanlobby-cli/internal/version

printf '%s\n' "-X $PACKAGE.gitVersion=$BUILD_TAG -X $PACKAGE.gitCommit=$GitCommit -X $PACKAGE.gitTreeState=$GitTreeState -X $PACKAGE.buildDate=$BuildDate"
