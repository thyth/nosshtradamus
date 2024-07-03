#!/usr/bin/env bash

PROJECT_DIR=$(dirname "$0")
GO_MOD_CACHE=$(go env GOMODCACHE) || exit 1

# Resolve the version of the go-mosh module we want
GO_MOSH_REPO="gitlab.hive.thyth.com/chronostruct/go-mosh"
GO_MOSH_VERSION=$(grep -oP "require ${GO_MOSH_REPO} \Kv(.*)" ${PROJECT_DIR}/go.mod)
GO_MOSH_DIR="${GO_MOD_CACHE}/${GO_MOSH_REPO}@${GO_MOSH_VERSION}"

# Ordinarily, the module cache contains version snapshots only. Replace the go-mosh snapshot with the full git checkout.
if [[ ! -d "${GO_MOSH_DIR}/.git" || -n FORCE_GO_MOSH_BUILD ]]; then
  echo "Rebuilding go-mosh@${GO_MOSH_VERSION} ..."
  rm -Rf ${GO_MOSH_DIR}
  mkdir -p ${GO_MOSH_DIR}

  # check out the specified version and the mosh code itself via submodules
  git clone -b ${GO_MOSH_VERSION} "https://${GO_MOSH_REPO}.git" ${GO_MOSH_DIR}
  git -C ${GO_MOSH_DIR} submodule update --init

  # delegate mosh build to the go-mosh build script; on failure, nuke the git dir so we try again next time
  ${GO_MOSH_DIR}/build-mosh.sh || (rm -Rf "${GO_MOSH_DIR}/.git" && exit 1)
fi

echo "go-mosh@${GO_MOSH_VERSION} is ready!"
