#!/usr/bin/env bash
# crawl.sh — trigger a scrapr crawl from the command line (for testing/dev).
#
# It mints a short-lived holistic session token from the shared JWT secret (needs sudo to read
# it), creates a scraper for the given URL, triggers it synchronously, and prints the resulting
# run + documents. This is the quickest way to exercise scrapr end-to-end without the browser.
#
# Usage:  scripts/crawl.sh <url> [name] [model]
#   <url>    seed URL to crawl, e.g. https://de.wikipedia.org/wiki/Graphentheorie
#   [name]   scraper name          (default: derived from the URL)
#   [model]  scraper model         (default: website)
#
# Env overrides:
#   SCRAPR_URL   base URL of the daemon      (default http://127.0.0.1:8782)
#   SCRAPR_USER  holistic username to auth as (default: current OS user; must be a real account)
#   HOLISTIC_SECRET_FILE  shared JWT secret   (default /etc/holistic/jwt-secret)
set -euo pipefail

URL="${1:?usage: crawl.sh <url> [name] [model]}"
NAME="${2:-scrapr test — $URL}"
MODEL="${3:-website}"
BASE="${SCRAPR_URL:-http://127.0.0.1:8782}/api/services/scrapr"
USER_NAME="${SCRAPR_USER:-$(id -un)}"
SECRET_FILE="${HOLISTIC_SECRET_FILE:-/etc/holistic/jwt-secret}"

# --- mint an h_access JWT (HS256) signed with the shared holistic secret ---
SECRET="$(sudo cat "$SECRET_FILE" | tr -d '\n\r')"
b64() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
hdr="$(printf '{"alg":"HS256","typ":"JWT"}' | b64)"
exp="$(( $(date +%s) + 900 ))"
pl="$(printf '{"sub":"%s","type":"access","exp":%d}' "$USER_NAME" "$exp" | b64)"
sig="$(printf '%s.%s' "$hdr" "$pl" | openssl dgst -sha256 -hmac "$SECRET" -binary | b64)"
TOK="$hdr.$pl.$sig"
AUTH=(-b "h_access=$TOK; h_csrf=c" -H "X-CSRF-Token: c")

# --- create the scraper ---
echo "→ creating scraper for $URL"
BODY="$(URL="$URL" NAME="$NAME" MODEL="$MODEL" python3 -c \
  'import json,os;print(json.dumps({"name":os.environ["NAME"],"model":os.environ["MODEL"],"source":os.environ["URL"],"scheduleKind":"manual","enabled":True}))')"
CREATE="$(curl -fsS "${AUTH[@]}" -H 'Content-Type: application/json' -X POST "$BASE/scrapers" -d "$BODY")"
ID="$(printf '%s' "$CREATE" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
echo "  scraper id: $ID"

# --- trigger the crawl (synchronous) ---
echo "→ crawling (synchronous; up to the crawl deadline)…"
curl -fsS --max-time 180 "${AUTH[@]}" -X POST "$BASE/scrapers/$ID/trigger" | python3 -c '
import json,sys
d=json.load(sys.stdin)
print("  status:", d.get("status"), "  documents added:", d.get("added"))
for x in d.get("documents",[]):
    print("   - [" + str(x.get("kategorie")) + "] " + str(x.get("title",""))[:72])
'
echo "✓ done."
