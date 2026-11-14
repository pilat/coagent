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
adr_files=$(mktemp)
adr_findings=$(mktemp)
trap 'rm -f "$packages" "$adr_files" "$adr_findings"' EXIT HUP INT TERM

go list -f '{{if or .GoFiles .CgoFiles}}{{.ImportPath}}{{end}}' ./... | sed '/^$/d' | sed "s|^$module/||" | sed '/^ai\//d' | sort > "$packages"

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
	"### Configuration apply and boot rollback" \
	"### Recovery and root-only publication" \
	"## Security and Trust Boundaries" \
	"### Credential boundary" \
	"### Filesystem boundary" \
	"### Local control boundary"
do
	if ! grep -Fqx "$heading" "$document"; then
		echo "$document is missing required heading: $heading"
		exit 1
	fi
done

adr_dir=docs/adr
adr_readme=$adr_dir/README.md
adr_template=$adr_dir/TEMPLATE.md

if ! grep -Fqx -- '- **Status:** Proposed | Accepted | Superseded by [ADR-NNNN](NNNN-slug.md)' "$adr_template"; then
	echo "$adr_template: the Status line changed; update the ADR check in $0"
	exit 1
fi

find "$adr_dir" -maxdepth 1 -type f -name '*.md' ! -name README.md ! -name TEMPLATE.md > "$adr_files"

# POSIX awk only (CI may run mawk): no {n} intervals, no gawk functions; numbers stay
# four-digit strings because some awks read "0010" as octal.
awk -v readme="$adr_readme" '
	function complain(message) { print message }

	function is_relation(name) { return name == "Supersedes" || name == "Amends" || name == "Amended by" }

	function end_field(file) {
		if (is_relation(field) && field_links == 0)
			complain(file ": " field " needs a [ADR-NNNN](NNNN-slug.md) link")
		field = ""
		field_links = 0
	}

	function check_bare_references(file, text) {
		gsub(/\[[^]]*\]\([^)]*\)/, "", text)
		if (text ~ /ADR-[0-9]|[0-9][0-9][0-9][0-9]-[^ ]*\.md/)
			complain(file ": " field " names an ADR outside a [ADR-NNNN](NNNN-slug.md) link: " text)
	}

	function add_links(file, key, text,    label, target) {
		while (match(text, /\[[^]]*\]\([^)]*\)/)) {
			field_links++
			label = substr(text, RSTART + 1, RLENGTH - 2)
			target = label
			sub(/^.*\]\(/, "", target)
			sub(/\].*$/, "", label)
			text = substr(text, RSTART + RLENGTH)
			if (label !~ /^ADR-[0-9][0-9][0-9][0-9]$/ || target !~ /^[0-9][0-9][0-9][0-9]-[a-z0-9-]*\.md$/) {
				complain(file ": " key " link [" label "](" target ") must have the form [ADR-NNNN](NNNN-slug.md)")
			} else if (substr(label, 5) != substr(target, 1, 4)) {
				complain(file ": " key " link [" label "](" target ") names a different number than its target")
			} else {
				links[file SUBSEP key SUBSEP target] = 1
			}
		}
	}

	function parse_header(path, file,    line, link_key, text, statuses) {
		field = ""
		field_links = 0
		statuses = 0
		if ((getline line < path) <= 0) { complain(file ": cannot read"); return }
		if (index(line, "# ADR-" substr(file, 1, 4) ": ") != 1)
			complain(file ": first line must start with # ADR-" substr(file, 1, 4) ":")
		while ((getline line < path) > 0) {
			if (line ~ /^## /) break
			if (line ~ /^- \*\*[^*]*:\*\*/) {
				end_field(file)
				field = line
				sub(/^- \*\*/, "", field)
				sub(/:\*\*.*$/, "", field)
				text = line
				sub(/^- \*\*[^*]*:\*\*[ ]*/, "", text)
				if (field == "Status") {
					statuses++
					check_status(file, text)
				} else if (field == "Date") {
					if (text !~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]$/)
						complain(file ": Date must be a bare YYYY-MM-DD: " text)
				} else if (!is_relation(field)) {
					complain(file ": unknown header field " field)
				}
			} else if (line ~ /^  [^ ]/ && field != "" && field != "Date" && field != "Status") {
				text = line
			} else {
				end_field(file)
				if (line != "") complain(file ": header line is not - **Field:** value: " line)
				continue
			}
			if (is_relation(field)) check_bare_references(file, text)
			link_key = (field == "Status") ? "Superseded by" : field
			add_links(file, link_key, text)
		}
		end_field(file)
		close(path)
		if (statuses != 1) complain(file ": needs exactly one - **Status:** line; found " statuses)
	}

	function check_status(file, text) {
		if (text == "Proposed" || text == "Accepted") return
		if (text ~ /^Superseded by \[ADR-[0-9][0-9][0-9][0-9]\]\([^)]*\)(: .*)?$/) return
		complain(file ": Status \"" text "\" is not a value docs/adr/TEMPLATE.md allows")
	}

	function require_back_links(forward, back,    triple, part) {
		for (triple in links) {
			split(triple, part, SUBSEP)
			if (part[2] != forward || !(part[3] in exists)) continue
			if (!((part[3] SUBSEP back SUBSEP part[1]) in links))
				complain(part[1] ": " forward " " part[3] ", but " part[3] " has no " back " link back to " part[1])
		}
	}

	BEGIN {
		section = ""
		while ((getline line < readme) > 0) {
			if (line ~ /^## /) {
				section = line
			} else if (section == "## Burned numbers" && line ~ /^- [0-9][0-9][0-9][0-9](-[0-9][0-9][0-9][0-9])?: /) {
				first = substr(line, 3, 4)
				last = (substr(line, 7, 1) == "-") ? substr(line, 8, 4) : first
				for (n = 1; n <= 9999; n++) {
					candidate = sprintf("%04d", n)
					if (candidate >= first && candidate <= last) burned[candidate] = 1
				}
			} else if (section == "## Shared numbers") {
				shared = shared line "\n"
			}
		}
		close(readme)
		highest = ""
	}

	{
		path = $0
		file = path
		sub(/^.*\//, "", file)
		if (file !~ /^[0-9][0-9][0-9][0-9]-[a-z0-9][a-z0-9-]*\.md$/) {
			complain(file ": ADR filenames must be NNNN-kebab-case-title.md")
			next
		}
		exists[file] = 1
		number = substr(file, 1, 4)
		if (number == "0000") complain(file ": ADR numbers start at 0001")
		if (number > highest) highest = number
		count[number]++
		if (number in burned) complain(file ": " number " is burned in " readme " and is never reused")
		if (count[number] == 1) first_holder[number] = file
		if (count[number] == 2 && index(shared, "`" first_holder[number] "`") == 0)
			complain(first_holder[number] ": shares " number " with another ADR; only the pairs in " readme " may")
		if (count[number] >= 2 && index(shared, "`" file "`") == 0)
			complain(file ": shares " number " with another ADR; only the pairs in " readme " may")
		parse_header(path, file)
	}

	END {
		for (triple in links) {
			split(triple, part, SUBSEP)
			if (!(part[3] in exists)) complain(part[1] ": " part[2] " link points at " part[3] ", which does not exist")
		}
		for (n = 1; (candidate = sprintf("%04d", n)) <= highest; n++)
			if (!(candidate in count) && !(candidate in burned))
				complain(candidate ": number gap; take the next free number, or list it under Burned numbers in " readme)
		require_back_links("Supersedes", "Superseded by")
		require_back_links("Superseded by", "Supersedes")
		require_back_links("Amends", "Amended by")
		require_back_links("Amended by", "Amends")
	}
' "$adr_files" > "$adr_findings"

if [ -s "$adr_findings" ]; then
	LC_ALL=C sort "$adr_findings"
	exit 1
fi
