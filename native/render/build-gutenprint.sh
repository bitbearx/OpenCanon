#!/bin/sh
# SPDX-License-Identifier: GPL-2.0-or-later
# 构建固定源码并修复上游行指针数组，不依赖系统未修补的动态库。
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
build_dir=$1
prefix=$2
case "$build_dir:$prefix" in
    /*:/*) ;;
    *) echo "build directory and Gutenprint prefix must be absolute" >&2; exit 1 ;;
esac
version=5.3.4.20220624T01008808d602
archive="gutenprint_${version}.orig.tar.xz"
url="https://deb.debian.org/debian/pool/main/g/gutenprint/$archive"
sha256=ef514d4ca567b5871cfe7697127bee2c14d5d71d7fbc68d8fee96b0faaed90e7
mkdir -p "$build_dir"
cd "$build_dir"
if [ ! -f "$archive" ]; then
    curl --fail --location --proto '=https' --tlsv1.2 --output "$archive.part" "$url"
    mv "$archive.part" "$archive"
fi
printf '%s  %s\n' "$sha256" "$archive" | sha256sum --check --strict
source_dir="$build_dir/gutenprint-5.3.4-2022-06-24T01-00-8808d602"
# 每次需要重建时从已校验归档恢复，避免在旧补丁或旧配置上继续构建。
if [ -d "$source_dir" ]; then rm -rf "$source_dir"; fi
tar -xJf "$archive"
cd "$source_dir"
patch --batch --fuzz=0 -p1 < "$script_dir/gutenprint-buffer.patch"
./configure --prefix="$prefix" --libdir="$prefix/lib" \
    --disable-shared --enable-static --with-modules=static \
    --without-cups --without-gimp2 --disable-libgutenprintui2 \
    --without-doc --disable-nls --without-readline \
    --disable-escputil --disable-test --disable-testpattern
make -j "${GUTENPRINT_JOBS:-2}"
make install
# 交付可重建的完整上游源码、修改、构建入口与原始版权文件。
source_docs="$prefix/share/doc/opencanon-render"
mkdir -p "$source_docs"
install -m 0644 "$build_dir/$archive" "$script_dir/gutenprint-buffer.patch" \
    "$script_dir/build-gutenprint.sh" "$script_dir/Makefile" \
    "$script_dir/opencanon-render.c" "$script_dir/regression.py" \
    "$script_dir/COPYING" "$script_dir/NOTICE" "$source_docs/"
install -m 0644 COPYING "$source_docs/Gutenprint-COPYING"
install -m 0644 AUTHORS "$source_docs/Gutenprint-AUTHORS"
