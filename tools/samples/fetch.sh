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
# raw.pixls.us (current cameras: Nikon NEF, Sony ARW, Canon CR2 and CR3):
# CC0 (public domain dedication).
set -eu
dir=${1:?usage: fetch.sh DIR}
mkdir -p "$dir"
rawpy=https://raw.githubusercontent.com/letmaik/rawpy/a39c2e7a44911889c3360891012f862f904ba551/test
pp=https://raw.githubusercontent.com/photoprism/photoprism/fd375b3ca7ddd4d58e6f46f895a64a27734dc1b2/assets/samples
px=https://raw.pixls.us/data
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
40a9df99865ad25a68b1131014cf954fb72da8bc32991b6a5e19d14937e21bc9 $px/Nikon/D810/DSC_1882.NEF
0c9438258ddae5260b9f5a1aace6658a723f1cb1acace3a6cb507116dc6da4f2 $px/Nikon/D850/Nikon-D850-14bit-lossless-compressed.NEF
dc83d3e49db54ca9c3a1e41dc847afd213c822d482c40158e42d01ee8968f33b $px/Nikon/Z%207_2/Z72_0027.NEF
c13a8675294456b9c979e6c1e556951654f35377b2870af2874bfb556f21ce40 $px/Nikon/Z%208/Nikon_Z8_raw_14_bit_lossless_compression.NEF
ea8b093cd11c946c5eeb501b775859e8e617ce1d79577abc41b233c339e4004c $px/Nikon/Z%208/Nikon_Z8_raw_high_efficiency_hight.NEF
99d0fe24a38f0f7623aeab21440d266a5ac09f9665cf5283f590337e84323c38 $px/Nikon/Z%209/Nikon_-_NIKON_Z_9_-_14bit_compressed_(Lossless).NEF
e113deb305d84153b26251ce259d1f839be1f36b048cbe83a4548cbda806c0f5 $px/Nikon/Z%209/Nikon_-_NIKON_Z_9_-_14bit_compressed_(Lossy_High_Efficiency_Star).NEF
17180e80555c149c5954a8a2f09459ede593b49731f1941f7094be105f3520e2 $px/NIKON%20CORPORATION/NIKON%20Z6_3/Nikon_Z6__3_Lossless_compressed_FX.NEF
10eea35531eb5427c171ab5158d064b79844a9fe039f8ca971b20c4f3be36cfd $px/NIKON%20CORPORATION/NIKON%20Z6_3/Nikon_Z6__3_High_Efficiency_Star_FX.NEF
04918dfe85dc1da51f875d22030f39776e99ffa2af3d210b83659933234688fa $px/Sony/ILCE-7RM4/DSC00395.ARW
c294c388ad9bccbee169f87b9ceb661001515fb039d63f587dcabba76df5636d $px/Sony/ILCE-7RM5/7RM5-LosslessCompressedLarge.ARW
33ec5187659f8af4589113afbd138ec8d1cd14f6999d36a12de87ad5d8c7d439 $px/SONY/ILCE-1M2/A1_07627_lossless_compressed_large_full_frame.ARW
85bfec149261ffbae90797e45caae5e204e1e61ac62b92783925b5ee0f267d76 $px/Canon/EOS%205D%20Mark%20IV/B13A0729.CR2
21430c36387efe65bd09ac0fcd724bfd959d93dd391ec811a1b741c8663becb5 $px/Canon/EOS%20R5/Canon_EOS_R5_RAW_ISO_100_nocrop_nodual.CR3
1a93f96556aabbc4a7260750ab0a191ec7926bb978fe8428bf3465121da730d6 $px/Canon/EOS%20R6%20Mark%20II/canon-r6m2-img_0346-raw-normal-full.cr3
295d46a4d5383895e63126f67000d4352a5bb425ee4d8b16eb6b6c284bdf7994 $px/Canon/EOS%20R3/_C3A1200.CR3
e47981d90142291a2bd7ba5a3dea51a25fff72ad3dad83dd2d2698f3ef5f59a8 $px/Canon/EOS%2090D/Canon-90D-RAW-ISO-100.CR3
LIST
echo "samples ready in $dir"
