#!/usr/bin/env bash
# Secret-leak guard: scan staged changes for real credentials before commit/push.
#
# Standalone scanner — does NOT hijack core.hooksPath (that is owned by Amazon
# git-defender, the enterprise secret scanner that must stay active for pushes
# to public aws-samples repos). Run manually as the project-specific layer:
#   bash scripts/scan-staged-secrets.sh
# Exits non-zero if a real GCS HMAC / AWS key / private key is staged.
set -euo pipefail

staged=$(git diff --cached --name-only --diff-filter=ACM)
[ -z "$staged" ] && exit 0

# Patterns for real secrets that must never enter git history.
#  - GCS HMAC access id:    GOOGL + 15+ uppercase/digits (real ids are 24 or 61 chars)
#  - AWS access key id:     AKIA/ASIA + 16 chars
#  - private keys:          PEM header
patterns='GOOGL[A-Z0-9]{15,}|A(KIA|SIA)[A-Z0-9]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----'

hits=$(git diff --cached -U0 -- $staged \
  | grep -E '^\+' \
  | grep -vE 'PLACEHOLDER|YOUR_|_HERE|EXAMPLE' \
  | grep -nE "$patterns" || true)

if [ -n "$hits" ]; then
  echo "🔴 pre-commit BLOCKED: 疑似真实凭证进入暂存区:" >&2
  echo "$hits" >&2
  echo "" >&2
  echo "凭证绝不入库。请改用 SecretsManager / 本地 .gitignore 文件。" >&2
  echo "确属误报可加占位符标记(PLACEHOLDER/EXAMPLE)或 git commit --no-verify(慎用)。" >&2
  exit 1
fi
exit 0
