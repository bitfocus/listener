#!/bin/bash
__DIRNAME="$(dirname "$(readlink -f "$0")")"
. $__DIRNAME/shared.sh

PROGRAM="bitfocus-listener"

# Scratch file for build output; /tmp is not writable in the same way on the
# Windows runners, so use mktemp.
BUILDLOG="$(mktemp)"

getVersion

if [ -z "$PRODUCTION_FOLDER" ]; then
	PRODUCTION_FOLDER="$__DIRNAME/../build"
fi

SHA1=$(git rev-parse --short HEAD)

WIN_VERSION="$VERSION_MAJOR.$VERSION_MINOR.$VERSION_PATCH.$VERSION_BUILD"

PLATFORM=$1
ARCH=$2

if [ -z "$PLATFORM" ]; then
	error "No platform provided"
fi

if [ -z "$ARCH" ]; then
	error "No arch provided"
fi

updateVersionFiles

case $PLATFORM in
	windows)
		NAME="$PROGRAM-win"
		# The windows build is a folder (exe + dlls + licenses), which CI zips up.
		EXT="zip"
		BINARY="$PROGRAM.exe"
		export GOOS=windows
		export EXTRAFLAGS="-ldflags -H=windowsgui"
		API_TARGET_X64="win-x64"
		API_TARGET_ARM64="win-arm64"
		;;
	mac)
		NAME="$PROGRAM-mac"
		EXT="dmg"
		BINARY="$PROGRAM"
		API_TARGET_X64="mac-intel"
		API_TARGET_ARM64="mac-arm"
		export GOOS=darwin
		export MACOSX_DEPLOYMENT_TARGET=14
		export CGO_LDFLAGS="$CGO_LDFLAGS -mmacosx-version-min=13.0"
		export CGO_CFLAGS="$CGO_CFLAGS -mmacosx-version-min=13.0"
		;;
	linux)
		error "Linux is not supported right now"
		;;
	*)
		error "Unknown platform $PLATFORM"
		;;
esac

case $ARCH in
	X64)
		export ARCH=amd64
		export GOARCH=amd64
		FILENAME="$NAME-x64"
		API_TARGET="$API_TARGET_X64"
		;;
	ARM64)
		ARCH=arm64
		export GOARCH=arm64
		FILENAME="$NAME-arm64"
		API_TARGET="$API_TARGET_ARM64"
		;;
	*)
		error "Unknown arch $ARCH"
		;;
esac

if [ "$BRANCH" == "stable" ]; then
	# example: bitfocus-listener-mac-x64-1.0.0.dmg"
	FILENAME="$FILENAME-$VERSION_MAJOR.$VERSION_MINOR.$VERSION_PATCH.$EXT"
else
	# example: bitfocus-listener-mac-x64-1.0.0-37-main-cec1269c.dmg
	# The + from the version is replaced with -, because the upload action
	# rewrites it in the s3 key and url but reports the raw filename to the api.
	FILENAME="$FILENAME-${VERSION//+/-}.$EXT"
fi

export CGO_ENABLED=1

rm -rf $__DIRNAME/../build
mkdir -p $__DIRNAME/../build

log "Building version $(_yellow $VERSION) for $(_yellow $PLATFORM)/$(_yellow $ARCH)"

export ICONDIR="$__DIRNAME/extras/"

# Generate syso file for Windows
if [ "$PLATFORM" = "windows" ]; then
	log Creating resource file for Windows
	# syso is a native Windows binary and cannot open the msys style paths that
	# readlink -f produces under Git Bash, so translate it first.
	if command -v cygpath >/dev/null 2>&1; then
		ICONDIR="$(cygpath -m "$ICONDIR")"
	fi
	sed -e "s/%FILENAME%/$PROGRAM/g" \
		-e "s/%VERSION%/$WIN_VERSION/g" \
		-e "s|%ICONDIR%|$ICONDIR|g" \
		-e "s/%COPYRIGHTYEAR%/$(date +%Y)/g" \
		tools/extras/syso/syso.json >syso.json
	cp tools/extras/syso/manifest.xml manifest.xml
	# go run avoids depending on GOBIN being on PATH, which differs between the
	# macOS, Linux and Windows runners.
	go run github.com/bitfocus/syso/cmd/syso@latest -arch $GOARCH >"$BUILDLOG" 2>&1 || error "Error generating resource file for golang application ${PROGRAM}: $(cat "$BUILDLOG")"

	log "Copying dynamic libraries and licenses"
	cp "$__DIRNAME"/extras/opengl32* "$PRODUCTION_FOLDER/"
	cp "$__DIRNAME"/../LICENSE "$PRODUCTION_FOLDER/LICENSE.txt"
	cp "$__DIRNAME"/../THIRD-PARTY-NOTICES.md "$PRODUCTION_FOLDER/"
fi

log "Building $(_yellow $PROGRAM) for $(_yellow $PLATFORM)/$(_yellow $ARCH)"
# The output filename is given explicitly: `-o <dir>` would name the binary after
# the module path (listener), but the app bundle and resources expect $PROGRAM.
go build -v $EXTRAFLAGS -o "$__DIRNAME/../build/$BINARY" . >"$BUILDLOG" 2>&1 || error "Failed to build ${PROGRAM}: $(cat "$BUILDLOG")"
rm -f "$BUILDLOG"

log "Built application"

if [ ! -z "$GITHUB_OUTPUT" ]; then
	# Export version information for GitHub Actions
	echo "VERSION_HASH=$VERSION_HASH" >>$GITHUB_OUTPUT
	echo "VERSION_MAJOR=$VERSION_MAJOR" >>$GITHUB_OUTPUT
	echo "VERSION_MINOR=$VERSION_MINOR" >>$GITHUB_OUTPUT
	echo "VERSION_PATCH=$VERSION_PATCH" >>$GITHUB_OUTPUT
	echo "VERSION_BUILD=$VERSION_BUILD" >>$GITHUB_OUTPUT
	echo "VERSION=$VERSION" >>$GITHUB_OUTPUT
	echo "API_TARGET=$API_TARGET" >>$GITHUB_OUTPUT
	echo "OUTPUT_FILENAME=$FILENAME" >>$GITHUB_OUTPUT
fi
