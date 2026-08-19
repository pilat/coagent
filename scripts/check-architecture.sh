#!/bin/sh

set -eu

document=ARCHITECTURE.md
module=github.com/pilat/coagent
max_lines=1800

line_count=$(wc -l < "$document")
if [ "$line_count" -gt "$max_lines" ]; then
	echo "$document has $line_count lines; maximum is $max_lines"
	exit 1
fi

packages=$(mktemp)
trap 'rm -f "$packages"' EXIT HUP INT TERM

go list -f '{{.ImportPath}}' ./... | sed "s|^$module/||" | sort > "$packages"

while IFS= read -r package; do
	marker="- \`$package\`"
	count=$(awk -v marker="$marker" 'index($0, marker) == 1 { n++ } END { print n + 0 }' "$document")
	if [ "$count" -ne 1 ]; then
		echo "$document must contain package-map marker '$marker' exactly once; found $count"
		exit 1
	fi
done < "$packages"

for heading in \
	"### Append-only transcript and compaction" \
	"### Pending external calls: producer ledger and exact result" \
	"### Subagent creation and completion" \
	"### Schedule delivery identity" \
	"### Configuration restart verdict" \
	"### Recovery and root-only publication" \
	"## Security and Trust Boundaries" \
	"### Credential boundary" \
	"### Filesystem and egress boundary" \
	"### Local control boundary"
do
	if ! grep -Fqx "$heading" "$document"; then
		echo "$document is missing required heading: $heading"
		exit 1
	fi
done
