#!/bin/bash

# Copyright Istio Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

GIT_USERNAME=${GIT_USERNAME:-"openshift-service-mesh-bot"}
GIT_EMAIL=${GIT_EMAIL:-""}
GIT_COMMIT_MESSAGE=${GIT_COMMIT_MESSAGE:-"Automated merge"}

MERGE_STRATEGY=${MERGE_STRATEGY:-"merge"}
MERGE_REPOSITORY=${MERGE_REPOSITORY:-"git@github.com:istio-ecosystem/sail-operator.git"}
MERGE_BRANCH=${MERGE_BRANCH:-"main"}

print_error() {
  local last_return="$?"

  {
    echo
    echo "${1:-unknown error}"
    echo
  } >&2

  return "${2:-$last_return}"
}

merge() {
  git remote add -f -t "$MERGE_BRANCH" upstream "$MERGE_REPOSITORY"
  echo "Using branch $MERGE_BRANCH"

  set +e # git returns a non-zero exit code on merge failure, which fails the script
  if [ "${MERGE_STRATEGY}" == "merge" ]; then
    git -c "user.name=$GIT_USERNAME" -c "user.email=$GIT_EMAIL" merge --no-ff -m "$GIT_COMMIT_MESSAGE" --log upstream/"$MERGE_BRANCH"
  else
    git -c "user.name=$GIT_USERNAME" -c "user.email=$GIT_EMAIL" rebase upstream/"$MERGE_BRANCH"
  fi
  return $?
}

main () {
  merge
  local code=$?
  set -e

  if [ "$code" -ne 0 ]; then
    echo "Conflicts detected, attempting to run 'make gen' to resolve."
    rm -rf bundle resources
    make gen
    git add bundle resources chart
    git -c "user.name=$GIT_USERNAME" -c "user.email=$GIT_EMAIL" commit --no-edit
    if [ "$?" -ne 0 ]; then
      print_error "Failed to resolve conflicts" $?
    fi
  fi
}

main