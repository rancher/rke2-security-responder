#!/bin/bash
# Asserts chart version invariants:
#   1. Chart.yaml `version` == `appVersion`
#   2. values.yaml `image.tag` == "v" + appVersion
#   3. (optional) RELEASE_TAG env var, when set, == "v" + appVersion
# Run from repo root. Used by pre-commit, CI, and release workflow.
set -euo pipefail

CHART="charts/rke2-security-responder/Chart.yaml"
VALUES="charts/rke2-security-responder/values.yaml"

extract() {
	# $1=file, $2=key (anchored at column 0)
	grep -E "^${2}:" "$1" | head -1 | sed -E "s/^${2}:[[:space:]]*\"?([^\"[:space:]]+)\"?.*/\1/"
}

extract_indented() {
	# $1=file, $2=key (with leading whitespace, e.g. image.tag)
	grep -E "^[[:space:]]+${2}:" "$1" | head -1 | sed -E "s/^[[:space:]]+${2}:[[:space:]]*\"?([^\"[:space:]]+)\"?.*/\1/"
}

chart_version=$(extract "${CHART}" version)
app_version=$(extract "${CHART}" appVersion)
image_tag=$(extract_indented "${VALUES}" tag)
expected_tag="v${app_version}"

fail() {
	echo "ERROR: $*" >&2
	echo "  ${CHART}:  version=${chart_version}  appVersion=${app_version}" >&2
	echo "  ${VALUES}: image.tag=${image_tag}" >&2
	echo "Bump them together so the chart references the published image." >&2
	exit 1
}

[[ "${chart_version}" == "${app_version}" ]] || fail "Chart.yaml version (${chart_version}) != appVersion (${app_version})"
[[ "${image_tag}" == "${expected_tag}" ]] || fail "values.yaml image.tag (${image_tag}) != expected (${expected_tag})"

if [[ -n "${RELEASE_TAG:-}" ]]; then
	[[ "${RELEASE_TAG}" == "${expected_tag}" ]] || fail "Git tag (${RELEASE_TAG}) != expected (${expected_tag})"
fi

echo "Chart version consistency OK: ${expected_tag}"
