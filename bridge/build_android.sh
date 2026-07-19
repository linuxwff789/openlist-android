#!/system/bin/sh
# Build OpenList Android native library (.so)
# Prerequisites: Android NDK, Go 1.21+
#
# Usage:
#   ANDROID_NDK_HOME=/path/to/ndk ./build_android.sh
#
# On Linux/macOS with NDK installed:
#   ./build_android.sh

set -e

: "${ANDROID_NDK_HOME:=$HOME/Android/ndk/27.0.12077973}"
: "${GOOS:=android}"
: "${GOARCH:=arm64}"
: "${OUTPUT:=$(pwd)/libopenlist.so}"

if [ ! -d "$ANDROID_NDK_HOME" ]; then
    echo "ERROR: NDK not found at $ANDROID_NDK_HOME"
    echo "Set ANDROID_NDK_HOME to your NDK path"
    echo "  e.g. ANDROID_NDK_HOME=$HOME/Android/ndk/27.0.12077973 ./build_android.sh"
    exit 1
fi

# Find NDK clang
case "$GOARCH" in
    arm64)
        TARGET=aarch64-linux-android
        ;;
    arm)
        TARGET=armv7a-linux-androideabi
        ;;
    amd64|x86_64)
        TARGET=x86_64-linux-android
        ;;
    *)
        echo "Unsupported GOARCH: $GOARCH"
        exit 1
        ;;
esac

# NDK clang path
NDK_BIN="$ANDROID_NDK_HOME/toolchains/llvm/prebuilt"
if [ -d "$NDK_BIN/darwin-x86_64" ]; then
    NDK_BIN="$NDK_BIN/darwin-x86_64"
elif [ -d "$NDK_BIN/linux-x86_64" ]; then
    NDK_BIN="$NDK_BIN/linux-x86_64"
elif [ -d "$NDK_BIN/windows-x86_64" ]; then
    NDK_BIN="$NDK_BIN/windows-x86_64"
else
    echo "Cannot find NDK prebuilt toolchain"
    exit 1
fi

# API level
API_LEVEL=21
CC="$NDK_BIN/bin/${TARGET}${API_LEVEL}-clang"

if [ ! -f "$CC" ]; then
    echo "C compiler not found: $CC"
    echo "Available compilers:"
    ls "$NDK_BIN/bin/" | grep clang | head -10
    exit 1
fi

echo "=== Building OpenList Android .so ==="
echo "NDK:      $ANDROID_NDK_HOME"
echo "CC:       $CC"
echo "GOOS:     $GOOS"
echo "GOARCH:   $GOARCH"
echo "Output:   $OUTPUT"
echo ""

cd "$(dirname "$0")/.."  # Go to OpenList project root

export GOOS=$GOOS
export GOARCH=$GOARCH
export CGO_ENABLED=1
export CC=$CC
export CGO_CFLAGS="-D__ANDROID_API__=$API_LEVEL -I$NDK_BIN/sysroot/usr/include"
export CGO_LDFLAGS="-D__ANDROID_API__=$API_LEVEL"

go build -buildmode=c-shared \
    -ldflags="-s -w -extldflags=-Wl,-soname,libopenlist.so" \
    -o "$OUTPUT" \
    -tags "glebarez" \
    ./bridge/

echo ""
echo "=== Build complete ==="
ls -lh "$OUTPUT"
file "$OUTPUT"
