#!/usr/bin/env bash
# Drives hack/docs-length.sh through the failures this tree does not
# contain: a page over its ceiling, and a page named in the table that does
# not exist. The other two cases -- under and exactly at -- are the shape
# every page in the tree is in right now, so they need fixtures too rather
# than reusing a real page that could grow past them tomorrow.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
check="$root/hack/docs-length.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

failures=0
run() {
  local name="$1" want="$2"; shift 2
  local out status
  out="$("$@" 2>&1)" && status=0 || status=$?
  if [ "$status" -ne "$want" ]; then
    echo "FAIL $name: exit $status, want $want"
    printf '%s\n' "$out" | sed 's/^/    /'
    failures=$((failures + 1))
  else
    echo "ok   $name"
  fi
}
says() {
  local name="$1" needle="$2"; shift 2
  local out
  out="$("$@" 2>&1)" || true
  case "$out" in
    *"$needle"*) echo "ok   $name" ;;
    *) echo "FAIL $name: output does not mention '$needle'"
       printf '%s\n' "$out" | sed 's/^/    /'
       failures=$((failures + 1)) ;;
  esac
}

# Ten words, so a ceiling of 9 is over and a ceiling of 10 is exactly at.
printf 'one two three four five six seven eight nine ten\n' > "$tmp/page.md"

run  "under its ceiling"    0 "$check" --page "$tmp/page.md:20"
run  "over its ceiling"     1 "$check" --page "$tmp/page.md:9"
says "names the file, count and ceiling" "$tmp/page.md: 10 words, ceiling 9" \
  "$check" --page "$tmp/page.md:9"
run  "exactly at its ceiling is not over" 0 "$check" --page "$tmp/page.md:10"

run  "a named file that does not exist" 1 "$check" --page "$tmp/absent.md:100"
says "names the missing file" "$tmp/absent.md does not exist" \
  "$check" --page "$tmp/absent.md:100"

if [ "$failures" -ne 0 ]; then
  echo "$failures case(s) failed"
  exit 1
fi
echo "all cases passed"
