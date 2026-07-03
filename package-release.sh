#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

plugin_id="codex-token-usage"
out_dir="${1:-dist}"
mkdir -p "${out_dir}"

bash ./build.sh

goos="$(go env GOOS)"
goarch="$(go env GOARCH)"
ext="so"
case "${goos}" in
  windows) ext="dll" ;;
  darwin) ext="dylib" ;;
esac

version="${PLUGIN_VERSION:-}"
if [[ -z "${version}" ]]; then
  version="$(sed -n 's/^[[:space:]]*pluginVersion[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' main.go | head -n 1)"
fi
if [[ -z "${version}" ]]; then
  echo "Cannot determine plugin version" >&2
  exit 1
fi

artifact="${plugin_id}.${ext}"
zip_name="${plugin_id}_${version}_${goos}_${goarch}.zip"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

cp "${artifact}" "${tmp_dir}/${artifact}"
if command -v zip >/dev/null 2>&1; then
  (cd "${tmp_dir}" && zip -9 -q "${OLDPWD}/${out_dir}/${zip_name}" "${artifact}")
else
  python3 - "${tmp_dir}" "${artifact}" "${out_dir}/${zip_name}" <<'PY'
import pathlib
import sys
import zipfile

tmp_dir = pathlib.Path(sys.argv[1])
artifact = sys.argv[2]
zip_path = pathlib.Path(sys.argv[3])
zip_path.parent.mkdir(parents=True, exist_ok=True)
with zipfile.ZipFile(zip_path, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as zf:
    zf.write(tmp_dir / artifact, artifact)
PY
fi

(
  cd "${out_dir}"
  : > checksums.txt
  if command -v sha256sum >/dev/null 2>&1; then
    for zip in *.zip; do
      sha256sum "${zip}" >> checksums.txt
    done
  else
    for zip in *.zip; do
      shasum -a 256 "${zip}" >> checksums.txt
    done
  fi
)

echo "Created ${out_dir}/${zip_name}"
echo "Created ${out_dir}/checksums.txt"
