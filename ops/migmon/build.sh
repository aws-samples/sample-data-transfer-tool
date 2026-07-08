#!/usr/bin/env bash
# build.sh — cross-compile migmon for both AL2023 architectures and (optionally)
# upload to ArtifactsBucket, matching the rclone/Lambda distribution model.
#
# 静态编译(CGO_ENABLED=0)保证 AL2023 直接跑,无 libc 依赖。产物:
#   dist/migmon-linux-amd64   (m6in x86_64 生产机型)
#   dist/migmon-linux-arm64   (c8gn/r7g arm64 生产机型)
#
# 用法:
#   ./build.sh                              # 只编译到 dist/
#   ./build.sh <ArtifactsBucket>            # 编译 + 上传 s3://<bucket>/ops/migmon-linux-<arch>
#   REGION=us-east-1 ./build.sh my-bucket   # 指定上传 region
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

BUCKET="${1:-}"
REGION="${REGION:-eu-south-2}"
LDFLAGS="-s -w"   # strip 符号表,减小体积
export CGO_ENABLED=0

mkdir -p dist
for arch in amd64 arm64; do
  out="dist/migmon-linux-${arch}"
  echo "→ 编译 $out"
  GOOS=linux GOARCH="$arch" go build -ldflags="$LDFLAGS" -o "$out" .
done
ls -lh dist/

if [ -n "$BUCKET" ]; then
  for arch in amd64 arm64; do
    key="ops/migmon-linux-${arch}"
    echo "→ 上传 s3://${BUCKET}/${key}"
    aws s3 cp "dist/migmon-linux-${arch}" "s3://${BUCKET}/${key}" --region "$REGION"
  done
  echo "✓ 已上传。机器上拉取用法(按 CpuArch 选架构):"
  echo "    aws s3 cp s3://${BUCKET}/ops/migmon-linux-\$(uname -m | sed s/x86_64/amd64/;s/aarch64/arm64/) /usr/local/bin/migmon && chmod +x /usr/local/bin/migmon"
fi
