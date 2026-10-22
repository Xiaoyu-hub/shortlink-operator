#!/usr/bin/env bash
# 在本机（Windows + Git Bash）重生成 CRD 与 RBAC 清单。
# 为什么不用 make manifests：chocolatey 的 GnuWin32 make 跑不动这套 Makefile。
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 1

CG=./bin/controller-gen-v0.22.0.exe
PKG=$(find cmd api internal pkg -type f -name '*.go' 2>/dev/null | xargs -n1 dirname 2>/dev/null | sort -u | sed 's|^|paths=./|')

echo "==> controller-gen $PKG"
# shellcheck disable=SC2086
"$CG" rbac:roleName=manager-role crd webhook $PKG output:crd:artifacts:config=config/crd/bases
rc=$?
echo "controller-gen exit=$rc"
exit $rc
