#!/bin/bash

# Mock environment
api_base="https://api.github.com/repos/full-bars/urnetwork-3.23-fix"

get_version_from_api_response () 
{    
    if command -v jq > /dev/null; then
        echo "$1" | tr -d '\000-\037' | jq -r '.tag_name' 2>/dev/null
    else
        echo "$1" | tr -d '\000-\037' | python3 -c 'import sys, json;
try:
    data = json.load(sys.stdin)
    print(data["tag_name"])
except (json.JSONDecodeError, KeyError):
    print("")' 2>/dev/null
    fi
}

echo "=========================================="
echo "    TEST 1: GitHub API Rate Limited       "
echo "=========================================="
# Simulate curl -f exiting with 22 on 403 Rate Limit and producing no stdout
network_fetch_rate_limited() {
    return 22 
}

api_url="$api_base/releases/latest"
# Our fix adds "|| true" so it doesn't crash here
release="$(network_fetch_rate_limited "$api_url" 2>/dev/null || true)"
latest_version="$(get_version_from_api_response "$release" 2>/dev/null)"

echo "Version extracted from API response: '$latest_version'"

# Our fallback logic:
if [ -z "$latest_version" ]; then
    echo "--> [Fallback triggered] API failed, using web scraping trick..."
    if command -v curl > /dev/null; then
        # Hit github.com directly (no unauthenticated rate limits)
        tag_url=$(curl -Ls -o /dev/null -w %{url_effective} "https://github.com/full-bars/urnetwork-3.23-fix/releases/latest")
        echo "--> tag_url resolved to: $tag_url"
        if [ -n "$tag_url" ] && [ "$tag_url" != "https://github.com/full-bars/urnetwork-3.23-fix/releases/latest" ]; then
            latest_version="${tag_url##*/}"
        fi
    fi
fi

echo "Final latest_version: '$latest_version'"
if case "$latest_version" in v3.23.0-fix*) true ;; *) false ;; esac; then
    echo "✅ TEST 1 PASSED: Resiliently extracted version!"
else
    echo "❌ TEST 1 FAILED"
fi

echo ""
echo "=========================================="
echo "    TEST 2: Normal API Response           "
echo "=========================================="
# Simulate normal API response
network_fetch_success() {
    echo '{"tag_name": "v3.23.0-fix.17"}'
}

release="$(network_fetch_success "$api_url" 2>/dev/null || true)"
latest_version="$(get_version_from_api_response "$release" 2>/dev/null)"

echo "Version extracted from API response: '$latest_version'"

if [ "$latest_version" = "v3.23.0-fix.17" ]; then
    echo "✅ TEST 2 PASSED: Extracted cleanly without fallback."
else
    echo "❌ TEST 2 FAILED"
fi
