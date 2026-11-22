#!/usr/bin/env bash
# Publishes one build to the rolling "latest" prerelease.
#
#   packaging/publish-latest.sh DIST_DIR NOTES_FILE
#
# Needs gh, and GH_TOKEN, GH_REPO, VERSION and the GITHUB_SHA, GITHUB_RUN_NUMBER,
# GITHUB_RUN_ID and GITHUB_RUN_ATTEMPT that Actions sets.
#
# The release never shows a half-updated build:
#   1. A run older than the published build exits without changing anything:
#      one with a lower run number (run numbers only grow, and a re-run keeps
#      its number and commit), or one whose commit is behind the tag.
#   2. This build's files are uploaded under a staging prefix and checked. A
#      failure here deletes the staged files and leaves the release as it was.
#   3. Each live file is renamed aside and its staged replacement renamed into
#      place, then the tag and notes move. If any step fails, every step
#      already taken is undone, so the old files, tag and notes all come back.
#      If an undo step fails too, nothing that might be serving a download is
#      deleted, and the script lists what is left and exits 2.
#   4. Only then are the old files, and any this build doesn't have, deleted.
set -Eeuo pipefail

if [[ $# -ne 2 ]]; then
    echo "usage: $0 DIST_DIR NOTES_FILE" >&2
    exit 2
fi
dist=$1 notes=$2
tag=latest
title="Latest build ($VERSION-$GITHUB_RUN_NUMBER)"
stage="staging-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT."
aside="previous-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The release body ends with a marker naming the build, so a later run can
# tell which build is published.
{ cat "$notes"; printf '\n<!-- prbot-build run=%s sha=%s -->\n' "$GITHUB_RUN_NUMBER" "$GITHUB_SHA"; } >"$work/body.md"

published_run() {
    sed -n 's/.*<!-- prbot-build run=\([0-9][0-9]*\) .*/\1/p' "$work/old-body.md" | tail -n 1
}

# --- 1. Skip a superseded run. ---------------------------------------------

old_sha=$(gh api "repos/$GH_REPO/git/ref/tags/$tag" --jq .object.sha 2>/dev/null || true)
[[ $old_sha =~ ^[0-9a-f]{40}$ ]] || old_sha=
have_release=
if gh release view "$tag" --json body,name --jq '.name' >"$work/old-title.txt" 2>/dev/null; then
    have_release=1
    gh release view "$tag" --json body --jq .body >"$work/old-body.md"
fi

# A higher published run number wins even over a commit that is ahead, which
# happens when main was reset to an older commit and published from there.
# The same run number is a re-run of the published build, which may republish.
run=
[[ -n $have_release ]] && run=$(published_run)
if [[ -n $run && $run -gt $GITHUB_RUN_NUMBER ]]; then
    echo "latest has run $run, newer than run $GITHUB_RUN_NUMBER; not publishing this run"
    exit 0
fi
# Without a recorded run, fall back to the commits.
if [[ -n $old_sha ]]; then
    status=$(gh api "repos/$GH_REPO/compare/$old_sha...$GITHUB_SHA" --jq .status 2>/dev/null || echo unknown)
    if [[ $status == behind ]]; then
        echo "latest is at $old_sha, which is newer than $GITHUB_SHA; not publishing this run"
        exit 0
    fi
fi

# A first publish creates the tag and an empty release to upload into.
if [[ -z $old_sha ]]; then
    gh api "repos/$GH_REPO/git/refs" -f ref="refs/tags/$tag" -f sha="$GITHUB_SHA" >/dev/null
fi
if [[ -z $have_release ]]; then
    gh release create "$tag" --verify-tag --prerelease --title "$title" --notes-file "$work/body.md"
fi
release_id=$(gh api "repos/$GH_REPO/releases/tags/$tag" --jq .id)

declare -A asset_id asset_size asset_state
load_assets() {
    asset_id=() asset_size=() asset_state=()
    local id name size state
    while IFS=$'\t' read -r id name size state; do
        asset_id[$name]=$id asset_size[$name]=$size asset_state[$name]=$state
    done < <(gh api --paginate "repos/$GH_REPO/releases/$release_id/assets?per_page=100" \
        --jq '.[] | [.id, .name, .size, .state] | @tsv')
}

rename_asset() {
    gh api -X PATCH "repos/$GH_REPO/releases/assets/$1" -f name="$2" >/dev/null
}

delete_asset() {
    gh api -X DELETE "repos/$GH_REPO/releases/assets/$1" >/dev/null
}

files=()
for f in "$dist"/*; do
    files+=("${f##*/}")
done

# --- 2. Stage this build's files. --------------------------------------------

discard_staged() {
    trap - ERR
    set +e
    echo "staging failed; deleting the staged files and leaving the release as it was" >&2
    load_assets
    local name
    for name in "${!asset_id[@]}"; do
        [[ $name == "$stage"* ]] && delete_asset "${asset_id[$name]}"
    done
    exit 1
}
trap discard_staged ERR

mkdir "$work/stage"
for name in "${files[@]}"; do
    cp "$dist/$name" "$work/stage/$stage$name"
done
gh release upload "$tag" "$work/stage"/*

load_assets
for name in "${files[@]}"; do
    want=$(stat -c %s "$dist/$name")
    if [[ ${asset_state[$stage$name]-} != uploaded || ${asset_size[$stage$name]-} != "$want" ]]; then
        echo "staged $name is ${asset_state[$stage$name]-missing}, ${asset_size[$stage$name]-0} bytes; expected uploaded, $want bytes" >&2
        false
    fi
done
declare -A staged_id live_id
for name in "${files[@]}"; do
    staged_id[$name]=${asset_id[$stage$name]}
    live_id[$name]=${asset_id[$name]-}
done

# --- 3. Swap the staged files in, then move the tag and notes. ---------------

moved_aside=() promoted=() tag_moved= notes_changed=
restore() {
    trap - ERR
    set +e
    echo "publishing failed; restoring the previous build" >&2
    local i name
    local -a incomplete=()
    local -A keep_staged=() still_live=()
    for ((i = ${#promoted[@]} - 1; i >= 0; i--)); do
        name=${promoted[i]}
        if ! rename_asset "${staged_id[$name]}" "$stage$name"; then
            # This build's file still holds the name and serves the download.
            keep_staged[$name]=1 still_live[$name]=1
            incomplete+=("$name: this build's file is still live")
        fi
    done
    for ((i = ${#moved_aside[@]} - 1; i >= 0; i--)); do
        name=${moved_aside[i]}
        if [[ -n ${still_live[$name]-} ]]; then
            incomplete+=("$name: the previous file is kept as $aside$name")
        elif ! rename_asset "${live_id[$name]}" "$name"; then
            keep_staged[$name]=1
            incomplete+=("$name: no live file; the previous one is kept as $aside$name and this build's as $stage$name")
        fi
    done
    if [[ -n $tag_moved ]] &&
        ! gh api -X PATCH "repos/$GH_REPO/git/refs/tags/$tag" -f sha="$old_sha" -F force=true >/dev/null; then
        incomplete+=("tag $tag: still at $GITHUB_SHA, was $old_sha")
    fi
    if [[ -n $notes_changed ]] &&
        ! gh release edit "$tag" --title "$(cat "$work/old-title.txt")" --notes-file "$work/old-body.md"; then
        incomplete+=("release notes: still describe run $GITHUB_RUN_NUMBER")
    fi
    for name in "${!staged_id[@]}"; do
        if [[ -z ${keep_staged[$name]-} ]]; then
            delete_asset "${staged_id[$name]}" ||
                echo "warning: couldn't delete staged asset $stage$name" >&2
        fi
    done
    if ((${#incomplete[@]})); then
        echo "::error::restoring the previous build is incomplete; re-run this job or fix these by hand:" >&2
        printf '  %s\n' "${incomplete[@]}" >&2
        exit 2
    fi
    echo "restored the previous build" >&2
    exit 1
}
trap restore ERR

for name in "${files[@]}"; do
    if [[ -n ${live_id[$name]} ]]; then
        rename_asset "${live_id[$name]}" "$aside$name"
        moved_aside+=("$name")
    fi
    rename_asset "${staged_id[$name]}" "$name"
    promoted+=("$name")
done

if [[ -n $old_sha && $old_sha != "$GITHUB_SHA" ]]; then
    gh api -X PATCH "repos/$GH_REPO/git/refs/tags/$tag" -f sha="$GITHUB_SHA" -F force=true >/dev/null
    tag_moved=1
fi
if [[ -n $have_release ]]; then
    notes_changed=1
    gh release edit "$tag" --prerelease --title "$title" --notes-file "$work/body.md"
fi
trap - ERR

# --- 4. Delete the old files and any this build doesn't have. -----------------

declare -A keep
for name in "${files[@]}"; do
    keep[$name]=1
done
load_assets
for name in "${!asset_id[@]}"; do
    if [[ -z ${keep[$name]-} ]]; then
        delete_asset "${asset_id[$name]}" || echo "warning: couldn't delete old asset $name" >&2
    fi
done
echo "published run $GITHUB_RUN_NUMBER ($GITHUB_SHA) to $tag"
