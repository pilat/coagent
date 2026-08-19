#!/bin/sh

set -eu

: "${VERSION:?VERSION must be an exact v-prefixed release version}"

if ! printf '%s\n' "$VERSION" | LC_ALL=C grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z][0-9A-Za-z.-]*)?$'; then
	echo "VERSION must look like v1.2.3 or v1.2.3-prerelease"
	exit 1
fi

if [ -n "${GITHUB_REF_NAME:-}" ] && [ "$GITHUB_REF_NAME" != "$VERSION" ]; then
	echo "VERSION $VERSION does not match tag $GITHUB_REF_NAME"
	exit 1
fi

if [ "${ALLOW_DIRTY:-0}" != "1" ]; then
	if [ -n "$(git status --porcelain)" ]; then
		echo "release builds require a clean worktree, including untracked files"
		exit 1
	fi
	tag_commit=$(git rev-list -n 1 "$VERSION" 2>/dev/null || true)
	if [ -z "$tag_commit" ] || [ "$tag_commit" != "$(git rev-parse HEAD)" ]; then
		echo "VERSION must name a tag at HEAD"
		exit 1
	fi
fi

output_dir=${OUTPUT_DIR:-dist}
epoch=${SOURCE_DATE_EPOCH:-$(git show -s --format=%ct HEAD)}

if [ -e "$output_dir" ] && [ ! -d "$output_dir" ]; then
	echo "OUTPUT_DIR exists and is not a directory: $output_dir"
	exit 1
fi
if [ -d "$output_dir" ] && [ -n "$(find "$output_dir" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
	echo "OUTPUT_DIR must be empty: $output_dir"
	exit 1
fi

release_tmp=$(mktemp -d)
trap 'rm -rf "$release_tmp"' EXIT HUP INT TERM

version_pkg=github.com/pilat/coagent/internal/version
ldflags="-buildid= -s -w -X $version_pkg.Version=$VERSION"

for platform in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64
do
	goos=${platform%-*}
	goarch=${platform#*-}
	CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
		GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build \
		-trimpath -buildvcs=false -ldflags "$ldflags" \
		-o "$release_tmp/$platform" ./cmd/coagent
done

GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go run -trimpath ./cmd/releasebuilder \
	-version "$VERSION" -epoch "$epoch" -out "$output_dir" -license LICENSE \
	"linux-amd64=$release_tmp/linux-amd64" \
	"linux-arm64=$release_tmp/linux-arm64" \
	"darwin-amd64=$release_tmp/darwin-amd64" \
	"darwin-arm64=$release_tmp/darwin-arm64"
