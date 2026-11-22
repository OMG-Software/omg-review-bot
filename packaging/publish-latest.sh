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
#      On a first publish, "as it was" is no release: a failure at any later
#      step deletes the release and tag the run created.
#   3. Each live file is renamed aside and its staged replacement renamed into
#      place, then the tag and notes move. If any step fails, every step
#      already taken is undone, so the old files, tag and notes all come back.
#      The undo reads the release as it is (a call that reported failure may
#      still have been applied), retries once, and deletes a staged copy only
#      once the release shows it isn't needed; otherwise it keeps every copy,
#      lists what is left and exits 2.
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
# Only without a recorded run, fall back to the commits: once main has been
# reset, a newer run can be on an older commit.
if [[ -z $run && -n $old_sha ]]; then
    status=$(gh api "repos/$GH_REPO/compare/$old_sha...$GITHUB_SHA" --jq .status 2>/dev/null || echo unknown)
    if [[ $status == behind ]]; then
        echo "latest is at $old_sha, which is newer than $GITHUB_SHA; not publishing this run"
        exit 0
    fi
fi

# A first publish creates the tag and an empty release to upload into. If it
# then fails, undo_create removes them again, so the repository is left as it
# was rather than with a release whose download links 404.
created_tag= created_release=
undo_create() {
    local left=()
    if [[ -n $created_release ]]; then
        gh release delete "$tag" --yes >/dev/null 2>&1
        gh release view "$tag" --json name >/dev/null 2>&1 && left+=("release $tag")
    fi
    if [[ -n $created_tag ]]; then
        gh api -X DELETE "repos/$GH_REPO/git/refs/tags/$tag" >/dev/null 2>&1
        gh api "repos/$GH_REPO/git/ref/tags/$tag" >/dev/null 2>&1 && left+=("tag $tag")
    fi
    if ((${#left[@]})); then
        echo "::error::couldn't remove what this first publish created; delete by hand:" >&2
        printf '  %s\n' "${left[@]}" >&2
        exit 2
    fi
    echo "removed the release and tag this first publish created" >&2
    exit 1
}
trap 'trap - ERR; set +e; undo_create' ERR
if [[ -z $old_sha ]]; then
    created_tag=1
    gh api "repos/$GH_REPO/git/refs" -f ref="refs/tags/$tag" -f sha="$GITHUB_SHA" >/dev/null
fi
if [[ -z $have_release ]]; then
    created_release=1
    gh release create "$tag" --verify-tag --prerelease --title "$title" --notes-file "$work/body.md"
fi
release_id=$(gh api "repos/$GH_REPO/releases/tags/$tag" --jq .id)
trap - ERR

# Reads the release's assets: asset_id, asset_size and asset_state by name, and
# asset_name by id. Fails, leaving the arrays empty, if the listing does.
declare -A asset_id asset_size asset_state asset_name
load_assets() {
    asset_id=() asset_size=() asset_state=() asset_name=()
    gh api --paginate "repos/$GH_REPO/releases/$release_id/assets?per_page=100" \
        --jq '.[] | [.id, .name, .size, .state] | @tsv' >"$work/assets.tsv" || return 1
    local id name size state
    while IFS=$'\t' read -r id name size state; do
        asset_id[$name]=$id asset_size[$name]=$size asset_state[$name]=$state asset_name[$id]=$name
    done <"$work/assets.tsv"
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
    [[ -n $created_release$created_tag ]] && undo_create
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

# The undo works from the release as it is now, not from a record of the
# steps taken: a call that reported failure may still have been applied.
restore() {
    trap - ERR
    set +e
    [[ -n $created_release$created_tag ]] && undo_create
    echo "publishing failed; restoring the previous build" >&2
    local attempt name old new cur
    local -a incomplete=()

    # Put every file back, then look again and retry once.
    for attempt in 1 2; do
        load_assets || continue
        for name in "${files[@]}"; do
            new=${staged_id[$name]} old=${live_id[$name]}
            if [[ ${asset_name[$new]-} == "$name" ]]; then
                rename_asset "$new" "$stage$name"
            fi
            if [[ -n $old && -n ${asset_name[$old]-} && ${asset_name[$old]} != "$name" ]]; then
                rename_asset "$old" "$name"
            fi
        done
    done

    # Delete a staged copy only once the release shows it's no longer needed;
    # if the release can't be read, keep every copy.
    if load_assets; then
        for name in "${files[@]}"; do
            new=${staged_id[$name]} old=${live_id[$name]}
            if [[ ${asset_name[$new]-} == "$name" ]]; then
                incomplete+=("$name: this build's file is still live${old:+; the previous one is ${asset_name[$old]:-missing}}")
            elif [[ -n $old && ${asset_name[$old]-} != "$name" ]]; then
                incomplete+=("$name: no live file; the previous one is ${asset_name[$old]:-missing} and this build's ${asset_name[$new]:-missing}")
            elif [[ -n ${asset_name[$new]-} ]]; then
                delete_asset "$new" || echo "warning: couldn't delete staged asset ${asset_name[$new]}" >&2
            fi
        done
    else
        incomplete+=("assets: couldn't read the release to check them; every staged and previous copy is kept")
    fi

    if [[ -n $old_sha ]]; then
        cur=$(gh api "repos/$GH_REPO/git/ref/tags/$tag" --jq .object.sha 2>/dev/null)
        if [[ $cur != "$old_sha" ]]; then
            gh api -X PATCH "repos/$GH_REPO/git/refs/tags/$tag" -f sha="$old_sha" -F force=true >/dev/null
            cur=$(gh api "repos/$GH_REPO/git/ref/tags/$tag" --jq .object.sha 2>/dev/null)
            [[ $cur == "$old_sha" ]] || incomplete+=("tag $tag: at ${cur:-unknown}, was $old_sha")
        fi
    fi
    if [[ -n $have_release ]] && ! notes_match; then
        gh release edit "$tag" --title "$(cat "$work/old-title.txt")" --notes-file "$work/old-body.md"
        notes_match || incomplete+=("release notes: not the previous build's")
    fi

    if ((${#incomplete[@]})); then
        echo "::error::restoring the previous build is incomplete; re-run this job or fix these by hand:" >&2
        printf '  %s\n' "${incomplete[@]}" >&2
        exit 2
    fi
    echo "restored the previous build" >&2
    exit 1
}

# Whether the release's title and notes are still the previous build's.
notes_match() {
    gh release view "$tag" --json name --jq .name >"$work/cur-title.txt" 2>/dev/null &&
        gh release view "$tag" --json body --jq .body >"$work/cur-body.md" 2>/dev/null &&
        cmp -s "$work/cur-title.txt" "$work/old-title.txt" && cmp -s "$work/cur-body.md" "$work/old-body.md"
}
trap restore ERR

for name in "${files[@]}"; do
    if [[ -n ${live_id[$name]} ]]; then
        rename_asset "${live_id[$name]}" "$aside$name"
    fi
    rename_asset "${staged_id[$name]}" "$name"
done

if [[ -n $old_sha && $old_sha != "$GITHUB_SHA" ]]; then
    gh api -X PATCH "repos/$GH_REPO/git/refs/tags/$tag" -f sha="$GITHUB_SHA" -F force=true >/dev/null
fi
if [[ -n $have_release ]]; then
    gh release edit "$tag" --prerelease --title "$title" --notes-file "$work/body.md"
fi
trap - ERR

# --- 4. Delete the old files and any this build doesn't have. -----------------

declare -A keep
for name in "${files[@]}"; do
    keep[$name]=1
done
if ! load_assets; then
    echo "warning: published, but couldn't list the assets to delete the old ones" >&2
    exit 0
fi
for name in "${!asset_id[@]}"; do
    if [[ -z ${keep[$name]-} ]]; then
        delete_asset "${asset_id[$name]}" || echo "warning: couldn't delete old asset $name" >&2
    fi
done
echo "published run $GITHUB_RUN_NUMBER ($GITHUB_SHA) to $tag"
