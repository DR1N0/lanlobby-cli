#!/usr/bin/env sh
# BUILD_TAG may be supplied by CI, for example BUILD_TAG=v0.0.1 make build.
set -eu

ROOT=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
# Version comes from git tags when BUILD_TAG is not set explicitly.
if [ -z "${BUILD_TAG:-}" ]; then
    if BUILD_TAG=$(git -C "$ROOT" describe --tags --always 2>/dev/null); then
        :
    else
        BUILD_TAG=dev
    fi
fi
# Keep every -X value a single linker argument, even when supplied by CI.
case "$BUILD_TAG" in
    *[!A-Za-z0-9._+-]*)
        printf 'invalid BUILD_TAG: use only letters, digits, dots, underscores, + and -\n' >&2
        exit 1
        ;;
esac

GitCommit=unknown
GitTreeState=unknown
if git -C "$ROOT" rev-parse --verify HEAD >/dev/null 2>&1; then
    GitCommit=$(git -C "$ROOT" rev-parse HEAD)
    if [ -n "$(git -C "$ROOT" status --porcelain --untracked-files=no)" ]; then
        GitTreeState=dirty
    else
        GitTreeState=clean
    fi
fi
BuildDate=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
PACKAGE=github.com/DR1N0/lanlobby-cli/internal/version

printf '%s\n' "-X $PACKAGE.gitVersion=$BUILD_TAG -X $PACKAGE.gitCommit=$GitCommit -X $PACKAGE.gitTreeState=$GitTreeState -X $PACKAGE.buildDate=$BuildDate"
