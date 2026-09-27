# Shared GitHub download helpers with strict timeout/retry policies.
# GitHub is reachable most of the time but its TLS edge can handshake OK yet
# stall (silent hang) or time out entirely in some regions; the helpers below
# bound each failure mode and retry independently:
#   - metadata fetches:   5s total, 20 tries   (small manifests/checksums)
#   - real content pulls: 10 tries, no total timeout — a big binary may
#                         legitimately take a while — but a transfer that
#                         connects and then crawls below 1 KiB/s for 30s is
#                         aborted so the next attempt can take over.
# Every helper removes a partial output on failure.
_dl_curl_once(){ # <max-time|0> <url> <outfile>
	local timeout="$1" url="$2" out="$3"
	# --connect-timeout bounds a dead or hanging TCP/TLS handshake.
	local args=(-fsSL --connect-timeout 10)
	if [[ "$timeout" == "0" ]]; then
		# No total cap: bound a stall instead of the whole transfer.
		args+=(--speed-limit 1024 --speed-time 30)
	else
		args+=(--max-time "$timeout")
	fi
	curl "${args[@]}" -o "$out" "$url" 2>/dev/null && return 0
	rm -f "$out"
	return 1
}

dl_meta(){ # <url> <outfile>  — metadata: 5s timeout, 20 tries
	local url="$1" out="$2" attempt
	for ((attempt=1; attempt<=20; attempt++)); do
		_dl_curl_once 5 "$url" "$out" && return 0
		(( attempt < 20 )) && sleep 1
	done
	rm -f "$out"
	return 1
}

dl_content(){ # <url> <outfile>  — large download: 10 tries, stall-bounded
	local url="$1" out="$2" attempt
	for ((attempt=1; attempt<=10; attempt++)); do
		_dl_curl_once 0 "$url" "$out" && return 0
		(( attempt < 10 )) && sleep 1
	done
	rm -f "$out"
	return 1
}

# sha256 of a release asset as recorded in the SHA256SUMS manifest.
# The manifest is written in text mode ("<sha>  <name>"), but a leading '*'
# from binary mode ("<sha> *<name>") is tolerated so a non-standard manifest
# still matches.
dl_release_sha(){
	local sums_file="$1" asset="$2"
	awk -v asset="$asset" '
		{ name=$NF; sub(/^\*/, "", name) }
		name==asset { print $1; found=1; exit }
		END { if (!found) exit 1 }' "$sums_file"
}
