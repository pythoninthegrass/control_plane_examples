#!/bin/bash

# Rate Limiting Test Suite
# Tests the complete rate limiting implementation

set -e  # Exit on error

API_URL="http://localhost:8080"
ADMIN_URL="http://localhost:8080"

# Colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Test counter
TESTS_PASSED=0
TESTS_FAILED=0

# Helper function to print test status
print_test() {
    echo -e "\n${YELLOW}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${YELLOW}TEST: $1${NC}"
    echo -e "${YELLOW}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
}

# Helper function to check if test passed
check_result() {
    if [ $1 -eq 0 ]; then
        echo -e "${GREEN}✓ PASSED${NC}"
        ((TESTS_PASSED++))
    else
        echo -e "${RED}✗ FAILED${NC}"
        ((TESTS_FAILED++))
    fi
}

# Helper function to check HTTP status
check_status() {
    local expected=$1
    local actual=$2
    local message=$3
    
    if [ "$expected" -eq "$actual" ]; then
        echo -e "${GREEN}✓ $message (HTTP $actual)${NC}"
        return 0
    else
        echo -e "${RED}✗ $message - Expected HTTP $expected, got HTTP $actual${NC}"
        return 1
    fi
}

# Wait for service to be ready
wait_for_service() {
    echo "Waiting for service to be ready..."
    for i in {1..30}; do
        if curl -s -f "$API_URL/health" > /dev/null 2>&1; then
            echo -e "${GREEN}✓ Service is ready${NC}"
            return 0
        fi
        echo -n "."
        sleep 1
    done
    echo -e "${RED}✗ Service failed to start${NC}"
    return 1
}

# Test IDs
TEST_CLIENT="test-client-$(date +%s)"
FREE_CLIENT="free-client-$(date +%s)"
PREMIUM_CLIENT="premium-client-$(date +%s)"
AUTO_CLIENT="auto-client-$(date +%s)"

echo "=================================="
echo "Rate Limiting Test Suite"
echo "=================================="
echo "API URL: $API_URL"
echo "Test started at: $(date)"
echo ""

# ============================================================================
# TEST 1: Service Health Check
# ============================================================================
print_test "1. Service Health Check"

wait_for_service
check_result $?

# ============================================================================
# TEST 2: Admin API - Get Available Tiers
# ============================================================================
print_test "2. Admin API - Get Available Tiers"

response=$(curl -s -w "\n%{http_code}" "$ADMIN_URL/admin/tiers")
body=$(echo "$response" | head -n -1)
status=$(echo "$response" | tail -n 1)

check_status 200 "$status" "Get tiers"
echo "Available tiers:"
echo "$body" | jq '.'
check_result $?

# ============================================================================
# TEST 3: Admin API - Create Free Tier Client
# ============================================================================
print_test "3. Admin API - Create Free Tier Client"

response=$(curl -s -w "\n%{http_code}" -X POST "$ADMIN_URL/admin/clients/set" \
  -H "Content-Type: application/json" \
  -d "{
    \"client_id\": \"$FREE_CLIENT\",
    \"tier\": \"free\",
    \"enabled\": true
  }")

body=$(echo "$response" | head -n -1)
status=$(echo "$response" | tail -n 1)

check_status 200 "$status" "Create free tier client"
echo "Created client:"
echo "$body" | jq '.'
check_result $?

# ============================================================================
# TEST 4: Admin API - Create Premium Tier Client
# ============================================================================
print_test "4. Admin API - Create Premium Tier Client"

response=$(curl -s -w "\n%{http_code}" -X POST "$ADMIN_URL/admin/clients/set" \
  -H "Content-Type: application/json" \
  -d "{
    \"client_id\": \"$PREMIUM_CLIENT\",
    \"tier\": \"premium\",
    \"enabled\": true
  }")

body=$(echo "$response" | head -n -1)
status=$(echo "$response" | tail -n 1)

check_status 200 "$status" "Create premium tier client"
echo "Created client:"
echo "$body" | jq '.'
check_result $?

# ============================================================================
# TEST 5: Admin API - Get Client Configuration
# ============================================================================
print_test "5. Admin API - Get Client Configuration"

response=$(curl -s -w "\n%{http_code}" "$ADMIN_URL/admin/clients/get?client_id=$FREE_CLIENT")
body=$(echo "$response" | head -n -1)
status=$(echo "$response" | tail -n 1)

check_status 200 "$status" "Get client config"
echo "Client config:"
echo "$body" | jq '.'
check_result $?

# ============================================================================
# TEST 6: Admin API - List All Clients
# ============================================================================
print_test "6. Admin API - List All Clients"

response=$(curl -s -w "\n%{http_code}" "$ADMIN_URL/admin/clients")
body=$(echo "$response" | head -n -1)
status=$(echo "$response" | tail -n 1)

check_status 200 "$status" "List clients"
client_count=$(echo "$body" | jq '.count')
echo "Total clients: $client_count"
echo "$body" | jq '.clients[] | {client_id, tier, rate_limit}'
check_result $?

# ============================================================================
# TEST 7: Enqueue Task - Auto-Create Client
# ============================================================================
print_test "7. Enqueue Task - Auto-Create Client (First Request)"

response=$(curl -s -w "\n%{http_code}" -X POST "$API_URL/v1/enqueue" \
  -H "Content-Type: application/json" \
  -d "{
    \"queue\": \"default\",
    \"client_id\": \"$AUTO_CLIENT\",
    \"task\": {
      \"url\": \"https://httpbin.org/post\",
      \"method\": \"POST\",
      \"body\": \"{\\\"test\\\": \\\"auto-create\\\"}\"
    }
  }")

body=$(echo "$response" | head -n -1)
status=$(echo "$response" | tail -n 1)

check_status 201 "$status" "Auto-create client and enqueue task"
echo "Response:"
echo "$body" | jq '.'

# Verify client was created with default tier
response2=$(curl -s -w "\n%{http_code}" "$ADMIN_URL/admin/clients/get?client_id=$AUTO_CLIENT")
body2=$(echo "$response2" | head -n -1)
status2=$(echo "$response2" | tail -n 1)

if [ "$status2" -eq 200 ]; then
    tier=$(echo "$body2" | jq -r '.tier')
    if [ "$tier" = "default" ]; then
        echo -e "${GREEN}✓ Client auto-created with default tier${NC}"
        check_result 0
    else
        echo -e "${RED}✗ Client created with wrong tier: $tier${NC}"
        check_result 1
    fi
else
    echo -e "${RED}✗ Client was not auto-created${NC}"
    check_result 1
fi

# ============================================================================
# TEST 8: Enqueue Task - Premium Client
# ============================================================================
print_test "8. Enqueue Task - Premium Client"

response=$(curl -s -w "\n%{http_code}" -X POST "$API_URL/v1/enqueue" \
  -H "Content-Type: application/json" \
  -d "{
    \"queue\": \"default\",
    \"client_id\": \"$PREMIUM_CLIENT\",
    \"task\": {
      \"url\": \"https://httpbin.org/post\",
      \"method\": \"POST\",
      \"body\": \"{\\\"test\\\": \\\"premium\\\"}\"
    }
  }")

body=$(echo "$response" | head -n -1)
status=$(echo "$response" | tail -n 1)

check_status 201 "$status" "Enqueue task for premium client"
echo "Response:"
echo "$body" | jq '.'

rate_limit=$(echo "$body" | jq -r '.rate_limit')
echo "Applied rate limit: $rate_limit"
check_result $?

# ============================================================================
# TEST 9: Rate Limit Enforcement - Free Tier (10 req/min)
# ============================================================================
print_test "9. Rate Limit Enforcement - Free Tier (10 req/min)"

echo "Sending 12 requests to free tier client (limit: 10/min)..."
hit_limit=false
requests_sent=0

for i in {1..12}; do
    response=$(curl -s -w "\n%{http_code}" -X POST "$API_URL/v1/enqueue" \
      -H "Content-Type: application/json" \
      -d "{
        \"queue\": \"default\",
        \"client_id\": \"$FREE_CLIENT\",
        \"task\": {
          \"url\": \"https://httpbin.org/post\",
          \"method\": \"POST\"
        }
      }")
    
    status=$(echo "$response" | tail -n 1)
    requests_sent=$i
    
    if [ "$status" -eq 429 ]; then
        echo -e "${GREEN}✓ Rate limit enforced at request $i${NC}"
        body=$(echo "$response" | head -n -1)
        echo "Rate limit response:"
        echo "$body" | jq '.'
        hit_limit=true
        break
    elif [ "$status" -eq 201 ]; then
        echo "  Request $i: HTTP 201 (accepted)"
    else
        echo -e "${RED}  Request $i: HTTP $status (unexpected)${NC}"
    fi
done

if [ "$hit_limit" = true ]; then
    check_result 0
else
    echo -e "${RED}✗ Rate limit was not enforced after $requests_sent requests${NC}"
    check_result 1
fi

# ============================================================================
# TEST 10: Per-Request Override
# ============================================================================
print_test "10. Per-Request Rate Limit Override"

response=$(curl -s -w "\n%{http_code}" -X POST "$API_URL/v1/enqueue" \
  -H "Content-Type: application/json" \
  -d "{
    \"queue\": \"default\",
    \"client_id\": \"$FREE_CLIENT\",
    \"rate_limit\": {
      \"max_concurrent\": 100,
      \"requests_per_min\": 1000
    },
    \"task\": {
      \"url\": \"https://httpbin.org/post\",
      \"method\": \"POST\",
      \"body\": \"{\\\"test\\\": \\\"override\\\"}\"
    }
  }")

body=$(echo "$response" | head -n -1)
status=$(echo "$response" | tail -n 1)

check_status 201 "$status" "Enqueue with rate limit override"

override_limit=$(echo "$body" | jq -r '.rate_limit.requests_per_min')
if [ "$override_limit" = "1000" ]; then
    echo -e "${GREEN}✓ Override applied: $override_limit req/min${NC}"
    check_result 0
else
    echo -e "${RED}✗ Override not applied: $override_limit req/min${NC}"
    check_result 1
fi

# ============================================================================
# TEST 11: Missing Client ID
# ============================================================================
print_test "11. Missing Client ID (Should Fail)"

response=$(curl -s -w "\n%{http_code}" -X POST "$API_URL/v1/enqueue" \
  -H "Content-Type: application/json" \
  -d "{
    \"queue\": \"default\",
    \"task\": {
      \"url\": \"https://httpbin.org/post\",
      \"method\": \"POST\"
    }
  }")

status=$(echo "$response" | tail -n 1)

check_status 401 "$status" "Reject request without client_id"
check_result $?

# ============================================================================
# TEST 12: Disable Client and Verify Rejection
# ============================================================================
print_test "12. Disable Client and Verify Rejection"

# Disable the client
response=$(curl -s -w "\n%{http_code}" -X POST "$ADMIN_URL/admin/clients/set" \
  -H "Content-Type: application/json" \
  -d "{
    \"client_id\": \"$TEST_CLIENT\",
    \"tier\": \"default\",
    \"enabled\": false
  }")

status=$(echo "$response" | tail -n 1)
check_status 200 "$status" "Disable client"

# Try to enqueue with disabled client
response=$(curl -s -w "\n%{http_code}" -X POST "$API_URL/v1/enqueue" \
  -H "Content-Type: application/json" \
  -d "{
    \"queue\": \"default\",
    \"client_id\": \"$TEST_CLIENT\",
    \"task\": {
      \"url\": \"https://httpbin.org/post\",
      \"method\": \"POST\"
    }
  }")

status=$(echo "$response" | tail -n 1)
check_status 401 "$status" "Reject disabled client"
check_result $?

# ============================================================================
# TEST 13: Delete Client
# ============================================================================
print_test "13. Admin API - Delete Client"

response=$(curl -s -w "\n%{http_code}" -X DELETE "$ADMIN_URL/admin/clients/delete?client_id=$TEST_CLIENT")
status=$(echo "$response" | tail -n 1)

check_status 204 "$status" "Delete client"
check_result $?

# Verify client is deleted
response=$(curl -s -w "\n%{http_code}" "$ADMIN_URL/admin/clients/get?client_id=$TEST_CLIENT")
status=$(echo "$response" | tail -n 1)

check_status 404 "$status" "Verify client deleted"
check_result $?

# ============================================================================
# TEST SUMMARY
# ============================================================================
echo ""
echo "=================================="
echo "TEST SUMMARY"
echo "=================================="
echo -e "${GREEN}Passed: $TESTS_PASSED${NC}"
echo -e "${RED}Failed: $TESTS_FAILED${NC}"
echo "Total: $((TESTS_PASSED + TESTS_FAILED))"
echo "=================================="
echo ""

# Cleanup test clients
echo "Cleaning up test clients..."
curl -s -X DELETE "$ADMIN_URL/admin/clients/delete?client_id=$FREE_CLIENT" > /dev/null 2>&1 || true
curl -s -X DELETE "$ADMIN_URL/admin/clients/delete?client_id=$PREMIUM_CLIENT" > /dev/null 2>&1 || true
curl -s -X DELETE "$ADMIN_URL/admin/clients/delete?client_id=$AUTO_CLIENT" > /dev/null 2>&1 || true
echo "Cleanup complete"

# Exit with appropriate code
if [ $TESTS_FAILED -eq 0 ]; then
    echo -e "${GREEN}All tests passed! ✓${NC}"
    exit 0
else
    echo -e "${RED}Some tests failed! ✗${NC}"
    exit 1
fi

