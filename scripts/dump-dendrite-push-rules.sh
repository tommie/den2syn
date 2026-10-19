#!/bin/bash
# dump-dendrite-push-rules.sh - Dumps every default push rule set Dendrite has shipped.
#
# Dendrite stores a copy of its default push rules when an account is created,
# so stored rules can match an old default rather than reflect a user's choice.
# den2syn compares against these to tell the two apart.
#
# Writes den2syn/dendrite_default_push_rules.json, mapping the commits that
# changed Dendrite's defaults to the rule set. The localpart and server name
# are placeholders.

set -euo pipefail

cd "$(dirname "$0")/.."

OUT=den2syn/dendrite_default_push_rules.json
WORK=/tmp/den2syn/dendrite-push-rules
mkdir -p "$WORK"

commits=$(git -C sources/dendrite log --format=%h -- internal/pushrules/default*.go)

echo "{" > "$OUT.tmp"
first=1
for commit in $commits; do
    tree="$WORK/$commit"
    if [ ! -d "$tree" ]; then
        git -C sources/dendrite worktree add --detach "$tree" "$commit" >/dev/null
    fi
    mkdir -p "$tree/cmd/den2syn-dump-push-rules"
    cat > "$tree/cmd/den2syn-dump-push-rules/main.go" <<'EOF'
package main

import (
	"encoding/json"
	"os"

	"github.com/matrix-org/dendrite/internal/pushrules"
)

func main() {
	// Untyped constants, since the server name type changed between versions.
	if err := json.NewEncoder(os.Stdout).Encode(pushrules.DefaultGlobalRuleSet("@LOCALPART@", "@SERVERNAME@")); err != nil {
		panic(err)
	}
}
EOF
    [ $first = 1 ] || echo "," >> "$OUT.tmp"
    first=0
    printf '"%s": ' "$commit" >> "$OUT.tmp"
    (cd "$tree" && go run ./cmd/den2syn-dump-push-rules) >> "$OUT.tmp"
done
echo "}" >> "$OUT.tmp"

python3 -I -c 'import json, sys; json.dump(json.load(open(sys.argv[1])), open(sys.argv[2], "w"), indent=1, sort_keys=True)' "$OUT.tmp" "$OUT"
rm "$OUT.tmp"

for commit in $commits; do
    git -C sources/dendrite worktree remove --force "$WORK/$commit"
done
