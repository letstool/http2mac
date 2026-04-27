#!/usr/bin/env bash
# 999_test.sh — smoke-test a running http2mac instance
# Usage: BASE_URL=http://localhost:8080 bash scripts/999_test.sh
set -euo pipefail

BASE="${BASE_URL:-http://localhost:8080}"
PASS=0; FAIL=0

ok()   { echo "  ✓ $1"; PASS=$((PASS+1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL+1)); }

assert_field() {
    local label="$1" body="$2" field="$3" expected="$4"
    local actual
    actual=$(echo "$body" | grep -o "\"$field\":[^,}]*" | head -1 | cut -d: -f2 | tr -d ' "')
    if [ "$actual" = "$expected" ]; then ok "$label ($field=$expected)"
    else fail "$label — $field: expected '$expected', got '$actual'"; fi
}

echo ""
echo "=== http2mac smoke tests against $BASE ==="
echo ""

# ── 1. Health / UI
echo "1. Static assets"
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/")
[ "$code" = "200" ] && ok "GET / → 200" || fail "GET / → $code"

# ── 2. OpenAPI
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/openapi.json")
[ "$code" = "200" ] && ok "GET /openapi.json → 200" || fail "GET /openapi.json → $code"

# ── 3. Single MAC (well-known Xerox OUI)
echo ""
echo "2. Single MAC lookup"
BODY=$(curl -s -X POST "$BASE/api/v1/lookup" \
    -H 'Content-Type: application/json' \
    -d '{"mac":"00:00:00:11:22:33"}')
assert_field "Xerox OUI (colon format)" "$BODY" "valid" "true"
assert_field "Xerox OUI — type"         "$BODY" "type" "Unicast"
assert_field "Xerox OUI — registered"   "$BODY" "registered" "true"

# ── 4. Format variants
echo ""
echo "3. MAC format variants"
for mac in "00-00-00-11-22-33" "00.00.00.11.22.33" "0000.0011.2233" "000000112233"; do
    BODY=$(curl -s -X POST "$BASE/api/v1/lookup" \
        -H 'Content-Type: application/json' \
        -d "{\"mac\":\"$mac\"}")
    valid=$(echo "$BODY" | grep -o '"valid":[^,}]*' | head -1 | cut -d: -f2 | tr -d ' "')
    [ "$valid" = "true" ] && ok "format: $mac" || fail "format: $mac (valid=$valid)"
done

# ── 5. Multicast detection
echo ""
echo "4. Multicast flag"
BODY=$(curl -s -X POST "$BASE/api/v1/lookup" \
    -H 'Content-Type: application/json' \
    -d '{"mac":"01:00:5e:00:00:01"}')
type_val=$(echo "$BODY" | grep -o '"type":"[^"]*"' | head -1 | cut -d'"' -f4)
[ "$type_val" = "Multicast" ] && ok "01:00:5e:... → Multicast" || fail "01:00:5e:... → type=$type_val"

# ── 6. Invalid MAC
echo ""
echo "5. Invalid MAC"
BODY=$(curl -s -X POST "$BASE/api/v1/lookup" \
    -H 'Content-Type: application/json' \
    -d '{"mac":"zz:zz:zz:zz:zz:zz"}')
valid=$(echo "$BODY" | grep -o '"valid":[^,}]*' | head -1 | cut -d: -f2 | tr -d ' "')
[ "$valid" = "false" ] && ok "invalid MAC → valid=false" || fail "invalid MAC → valid=$valid"

# ── 7. Batch
echo ""
echo "6. Batch lookup"
BODY=$(curl -s -X POST "$BASE/api/v1/lookup" \
    -H 'Content-Type: application/json' \
    -d '{"macs":["00:00:00:00:00:01","FF:FF:FF:FF:FF:FF"]}')
count=$(echo "$BODY" | grep -o '"mac"' | wc -l)
[ "$count" -ge 2 ] && ok "batch returned $count results" || fail "batch returned $count results (expected >=2)"

# ── 8. Summary
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] && exit 0 || exit 1
