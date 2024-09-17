#!/usr/bin/env bash
set -aeuo pipefail

# This script is used to validate the ssa feature, triggered by the
# uptest framework via `uptest.upbound.io/post-assert-hook`: https://github.com/crossplane/uptest/tree/e64457e2cce153ada54da686c8bf96143f3f6329?tab=readme-ov-file#hooks

# gets the directory of the this test hook script (POSIX-compliant)
# workaround for determining the filepath of the LABELER_OBJECT
# in chainsaw-based uptest v1.x versions
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)

LABELER_OBJECT="$script_dir/object-ssa-labeler.yaml"
${KUBECTL} apply -f "${LABELER_OBJECT}"
${KUBECTL} wait -f "${LABELER_OBJECT}" --for condition=ready --timeout=1m

if ${KUBECTL} get service sample-service -o jsonpath='{.metadata.annotations}' | grep "last-applied-configuration"; then # This annotation should not be present when SSA is enabled
  echo "SSA validation failed! Annotation 'last-applied-configuration' should not exist when SSA is enabled!"
  exit 1
fi
if ! (${KUBECTL} get service sample-service -o jsonpath='{.metadata.labels.some-key}' | grep -q "some-value" && ${KUBECTL} get service sample-service -o jsonpath='{.metadata.labels.another-key}' | grep -q "another-value"); then
  echo "SSA validation failed! Labels 'some-key' and 'another-key' from both Objects should exist with values 'some-value' and 'another-value' respectively!"
  exit 1
fi
echo "Successfully validated the SSA feature!"

${KUBECTL} delete -f "${LABELER_OBJECT}"
