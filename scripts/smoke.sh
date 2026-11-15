#!/usr/bin/env bash
# Smoke test for meshbay-sample-migrations.
#
#   scripts/smoke.sh <url> [expected-schema] [expected-release]
#
# expected-schema defaults to 2 (main). Pass 3 when the v3-add-tags branch is
# deployed, and optionally the release tag (v2, v3, v3-broken) as third argument.
set -euo pipefail

if [[ $# -lt 1 || -z "${1:-}" ]]; then
  echo "usage: scripts/smoke.sh <url> [expected-schema] [expected-release]" >&2
  exit 2
fi

baseUrl="${1%/}"
expectedSchema="${2:-2}"
expectedRelease="${3:-}"

if [[ ! "$expectedSchema" =~ ^[0-9]+$ ]]; then
  echo "FAIL expected-schema must be a number, got '$expectedSchema'" >&2
  exit 2
fi

itemName="smoke-$(date +%s)"
itemNote="note-${itemName}"
renamedName="${itemName}-renamed"
renamedNote="note-${renamedName}"
createBody="$(printf '{"name":"%s","note":"%s"}' "$itemName" "$itemNote")"
updateBody="$(printf '{"name":"%s","note":"%s"}' "$renamedName" "$renamedNote")"

responseFile="$(mktemp)"
trap 'rm -f "$responseFile"' EXIT

statusCode=""

showResponse() {
  if [[ -s "$responseFile" ]]; then
    echo "response: $(head -c 500 "$responseFile")" >&2
  fi
}

fail() {
  echo "FAIL $*" >&2
  showResponse
  exit 1
}

# request <curl arguments...>; the HTTP status lands in $statusCode and the body
# in $responseFile.
request() {
  statusCode="$(curl -s -m 20 -o "$responseFile" -w '%{http_code}' "$@")"
}

expectStatus() {
  local label="$1" expected="$2"
  if [[ "$statusCode" != "$expected" ]]; then
    echo "FAIL $label: expected HTTP $expected, got $statusCode" >&2
    showResponse
    exit 1
  fi
  echo "ok   $label ($statusCode)"
}

# --- health: deploy ran pre_deploy up to the expected schema version --------
request "$baseUrl/health"
expectStatus "GET /health" 200

jq -e . "$responseFile" > /dev/null 2>&1 || fail "/health did not return JSON"
healthStatus="$(jq -r '.status // empty' "$responseFile")"
schemaVersion="$(jq -r '.schema // empty' "$responseFile")"
releaseTag="$(jq -r '.release // empty' "$responseFile")"

[[ "$healthStatus" == "ok" ]] || fail "/health status is '$healthStatus', expected 'ok'"
[[ -n "$schemaVersion" ]] || fail "/health has no schema version"
[[ "$schemaVersion" == "$expectedSchema" ]] \
  || fail "schema version is $schemaVersion, expected $expectedSchema (pre_deploy did not run, or ran the wrong revision)"
if [[ -n "$expectedRelease" && "$releaseTag" != "$expectedRelease" ]]; then
  fail "release is '$releaseTag', expected '$expectedRelease' (is the old revision still serving?)"
fi
echo "ok   GET /health reports schema $schemaVersion, release ${releaseTag:-unknown}"

# --- CRUD on /api/items (migration 2 added the note column) ------------------
request -X POST -H 'content-type: application/json' -d "$createBody" "$baseUrl/api/items"
expectStatus "POST /api/items" 201
itemIdentifier="$(jq -r '.id // empty' "$responseFile")"
[[ -n "$itemIdentifier" ]] || fail "create returned no id"
[[ "$(jq -r '.name' "$responseFile")" == "$itemName" ]] || fail "create returned another name"
[[ "$(jq -r '.note' "$responseFile")" == "$itemNote" ]] || fail "create returned another note"

request "$baseUrl/api/items/$itemIdentifier"
expectStatus "GET /api/items/$itemIdentifier" 200
[[ "$(jq -r '.name' "$responseFile")" == "$itemName" ]] || fail "read returned another name"
[[ "$(jq -r '.note' "$responseFile")" == "$itemNote" ]] || fail "read returned another note"

request "$baseUrl/api/items"
expectStatus "GET /api/items lists it" 200
jq -e --arg wanted "$itemIdentifier" 'any(.[]; (.id | tostring) == $wanted)' "$responseFile" > /dev/null \
  || fail "list does not contain item $itemIdentifier"

request -X PUT -H 'content-type: application/json' -d "$updateBody" "$baseUrl/api/items/$itemIdentifier"
expectStatus "PUT /api/items/$itemIdentifier" 200
[[ "$(jq -r '.name' "$responseFile")" == "$renamedName" ]] || fail "update did not rename"
[[ "$(jq -r '.note' "$responseFile")" == "$renamedNote" ]] || fail "update did not rewrite the note"

request -X DELETE "$baseUrl/api/items/$itemIdentifier"
expectStatus "DELETE /api/items/$itemIdentifier" 204
request "$baseUrl/api/items/$itemIdentifier"
expectStatus "GET deleted item" 404

echo "OK schema=$schemaVersion release=${releaseTag:-unknown} on $baseUrl"
