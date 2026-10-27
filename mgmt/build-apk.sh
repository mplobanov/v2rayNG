#!/bin/bash
# Build the signed arm64 release APK of v2rayNG Family on macOS.
#
#   mgmt/build-apk.sh            -> V2rayNG/app/build/outputs/apk/playstore/release/*arm64-v8a.apk
#   ABIS="arm64-v8a armeabi-v7a" mgmt/build-apk.sh
#
# Inputs that never enter git (all under ~/.config/ngctl, written by mgmt/deploy/deploy.sh --init):
#   config.json + enroll_token + pin   management server baked into BuildConfig
#   v2rayng-family.jks + signing.properties   release key (back it up: losing it means family
#                                              devices cannot install updates over the old app)
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
conf="$HOME/.config/ngctl"
ABIS=${ABIS:-arm64-v8a}
export JAVA_HOME=${JAVA_HOME:-"/Applications/Android Studio.app/Contents/jbr/Contents/Home"}
export ANDROID_HOME=${ANDROID_HOME:-"$HOME/Library/Android/sdk"}
export NDK_HOME=${NDK_HOME:-"$ANDROID_HOME/ndk/30.0.16248370"}
cd "$root"

git submodule update --init --recursive

# hev-socks5-tunnel (JNI lib + root-mode binary), as in .github/workflows/build.yml
need_hev=0
for abi in $ABIS; do [[ -f "libs/$abi/libhev-socks5-tunnel.so" ]] || need_hev=1; done
if [[ $need_hev == 1 ]]; then ABIS="$ABIS" bash compile-hevtun.sh; fi
mkdir -p V2rayNG/app/libs
cp -R libs/. V2rayNG/app/libs/

# libv2ray.aar matching the AndroidLibXrayLite gitlink
tag=$(git -C AndroidLibXrayLite describe --tags --abbrev=0)
stamp=V2rayNG/app/libs/.libv2ray-tag
if [[ ! -f V2rayNG/app/libs/libv2ray.aar || "$(cat "$stamp" 2>/dev/null)" != "$tag" ]]; then
  gh release download "$tag" -R 2dust/AndroidLibXrayLite -p libv2ray.aar -D V2rayNG/app/libs --clobber
  echo "$tag" > "$stamp"
fi

# management server baked into the build
url=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["url"])' "$conf/config.json")
umask 077
printf 'url=%s\npin=%s\nenroll_token=%s\n' "$url" "$(cat "$conf/pin")" "$(cat "$conf/enroll_token")" > V2rayNG/mgmt.properties

# release key
if [[ ! -f "$conf/v2rayng-family.jks" ]]; then
  pass=$(openssl rand -hex 24)
  "$JAVA_HOME/bin/keytool" -genkeypair -keystore "$conf/v2rayng-family.jks" -alias family -keyalg RSA -keysize 4096 \
    -validity 36500 -storepass "$pass" -keypass "$pass" -dname "CN=v2rayNG Family" >/dev/null
  printf 'store_password=%s\n' "$pass" > "$conf/signing.properties"
  echo "created release key $conf/v2rayng-family.jks - back it up"
fi
pass=$(sed -n 's/^store_password=//p' "$conf/signing.properties")

echo "sdk.dir=$ANDROID_HOME" > V2rayNG/local.properties
cd V2rayNG
./gradlew assemblePlaystoreRelease --console=plain -q \
  -PABI_FILTERS="$(echo $ABIS | tr ' ' ';')" \
  -Pandroid.injected.signing.store.file="$conf/v2rayng-family.jks" \
  -Pandroid.injected.signing.store.password="$pass" \
  -Pandroid.injected.signing.key.alias=family \
  -Pandroid.injected.signing.key.password="$pass"
ls -la app/build/outputs/apk/playstore/release/*.apk
