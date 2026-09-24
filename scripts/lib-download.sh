# Shared GitHub download helpers with strict timeout/retry policies.
# GitHub is reachable most of the time but its TLS edge can handshake OK yet
# stall (silent hang) or time out entirely in some regions; the helpers below
# bound each failure mode and retry independently:
#   - metadata fetches:   5s total, 20 tries   (small manifests/checksums)
#   - handshake connects: 3s total, 20 tries   (probe headers/connectivity)
#   - real content pulls: 10 tries, no total timeout (large binary/repo tarball)
# Every helper removes a partial output on failure.
_dl_curl_once(){
	local timeout="$1" url="$2" out="$3"
	if [[ "$timeout" == "0" ]]; then
		curl -fsSL -o "$out" "$url" 2>/dev/null && return 0
	else
		curl -fsSL --max-time "$timeout" -o "$out" "$url" 2>/dev/null && return 0
	fi
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

dl_handshake(){ # <url> <outfile>  — connectivity probe: 3s timeout, 20 tries
	local url="$1" out="$2" attempt
	for ((attempt=1; attempt<=20; attempt++)); do
		_dl_curl_once 3 "$url" "$out" && return 0
		(( attempt < 20 )) && sleep 1
	done
	rm -f "$out"
	return 1
}

dl_content(){ # <url> <outfile>  — large download: 10 tries, no total timeout
	local url="$1" out="$2" attempt
	for ((attempt=1; attempt<=10; attempt++)); do
		_dl_curl_once 0 "$url" "$out" && return 0
		(( attempt < 10 )) && sleep 1
	done
	rm -f "$out"
	return 1
}

# sha256 of a release asset as recorded in the SHA256SUMS manifest.
dl_release_sha(){
	local sums_file="$1" asset="$2"
	awk -v asset="$asset" '$NF==asset{print $1; found=1; exit} END{if(!found) exit 1}' "$sums_file"
}
