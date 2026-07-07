#!/usr/bin/env bash

# Copyright AppsCode Inc. and Contributors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# structure-parity.sh diffs this repo's build skeleton against kubeops.dev/petset
# so the two cannot silently drift. It compares, after normalizing the product
# name (petset -> cattleset), the Makefile target list and the CI workflow job
# list. Only the name substitution is allowed to differ.
#
# Usage: PETSET_DIR=../petset hack/structure-parity.sh

set -eou pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PETSET_DIR="${PETSET_DIR:-${REPO_ROOT}/../petset}"

if [ ! -d "${PETSET_DIR}" ]; then
  echo "petset repo not found at ${PETSET_DIR}; set PETSET_DIR" >&2
  exit 1
fi

# normalize: strip the product name so only structure remains
normalize() {
  sed -e 's/cattleset/PRODUCT/g' -e 's/petset/PRODUCT/g' \
      -e 's/CattleSet/PRODUCT/g' -e 's/PetSet/PRODUCT/g'
}

# Makefile target names (lines like "target:")
targets() {
  grep -hoE '^[a-zA-Z0-9][a-zA-Z0-9_.-]*:' "$1/Makefile" | sort -u | normalize
}

# CI workflow job ids
jobs() {
  awk '/^jobs:/{f=1;next} f && /^  [a-zA-Z0-9_-]+:/{gsub(/[: ]/,"");print} /^[a-zA-Z]/{if(f && $0 !~ /^  /)f=0}' \
    "$1/.github/workflows/ci.yml" | sort -u | normalize
}

status=0

if ! diff <(targets "${PETSET_DIR}") <(targets "${REPO_ROOT}"); then
  echo "Makefile target list drifted from petset" >&2
  status=1
fi

if ! diff <(jobs "${PETSET_DIR}") <(jobs "${REPO_ROOT}"); then
  echo "CI workflow job list drifted from petset" >&2
  status=1
fi

if [ "${status}" -eq 0 ]; then
  echo "structure parity with petset: OK"
fi
exit "${status}"
