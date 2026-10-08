#!/bin/sh
# Download the real camera files that internal/imageio's sample test checks
# (TestRealSamples) into DIR, verifying each against its pinned SHA-256.
# They are not stored in this repository: they belong to others, under the
# licences noted below, and are only downloaded to run the tests.
#
#   tools/samples/fetch.sh DIR
#   ARCHIVIS_SAMPLES=DIR go test ./internal/imageio/ -run RealSamples
#
# rawpy (letmaik/rawpy, test files): the NEFs are NASA photographs from the
# International Space Station (public domain); the CR2s are from
# rawsamples.ch (CC BY-NC-SA 4.0).
# PhotoPrism (photoprism/photoprism, assets/samples): AGPL-3.0 project.
set -eu
dir=${1:?usage: fetch.sh DIR}
mkdir -p "$dir"
rawpy=https://raw.githubusercontent.com/letmaik/rawpy/a39c2e7a44911889c3360891012f862f904ba551/test
pp=https://raw.githubusercontent.com/photoprism/photoprism/fd375b3ca7ddd4d58e6f46f895a64a27734dc1b2/assets/samples
sha() { # sha256sum on Linux, shasum on macOS
	if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}
while read -r sum url; do
	f="$dir/$(basename "$url")"
	if [ ! -f "$f" ] || [ "$(sha "$f")" != "$sum" ]; then
		curl -fsSL --retry 3 -o "$f" "$url"
	fi
	[ "$(sha "$f")" = "$sum" ] || { echo "checksum mismatch: $f" >&2; exit 1; }
done <<LIST
add8183e643d98d5b07479d50bf5591838451d43449fd3399e2f146cf19beb8d $rawpy/M0054341_01_00005.cr2
152382ce4dbf644899d12b41b4c577f07638aa3ec5745ac344c36bad93826125 $rawpy/RAW_CANON_40D_SRAW_V103.CR2
0da28f6f2718ea82fdf95c35a1790597a3a7ecbe328a3f5b60a05f4e95e380e6 $rawpy/RAW_CANON_5DMARK2_PREPROD.CR2
5922721d13f11795557d97fdeb0a60b900086c402bc82a848ff280d15b99ffd4 $rawpy/iss030e122639.NEF
b21e9752aaf8e678fe4d8138986bc9819aa89b7b969a28d43c619af3bd6d58c1 $rawpy/iss042e297200.NEF
e5a7adfb8f7fbe43a31e637dab5bc161ed4af2c128f76bc1eb04162373893ecf $pp/canon_eos_6d.dng
c68e438f92341da271a731a059113db9395366f4c2f78ead5ded64deba7de463 $pp/iphone_15_pro.heic
2a2b5531ac2e1e3dd191f3386a09da2a39ce7f69b2d2dfd749c6c88129745040 $pp/iphone_7.heic
LIST
echo "samples ready in $dir"
