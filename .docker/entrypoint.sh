#!/bin/sh
set -e

# Docker actions run in the workspace but git may reject it as unsafe
git config --global --add safe.directory /github/workspace
cd /github/workspace

# If state-branch is set and state-path is not, default state-path to .sentinel-state
STATE_PATH="${INPUT_STATE_PATH}"
if [ -n "$INPUT_STATE_BRANCH" ] && [ -z "$STATE_PATH" ]; then
    STATE_PATH=".sentinel-state"
    INPUT_STATE_PATH="$STATE_PATH"
    export INPUT_STATE_PATH
fi

REPO_URL="https://x-access-token:${INPUT_GITHUB_TOKEN}@github.com/${GITHUB_REPOSITORY}"

# Restore state from branch before probing
if [ -n "$INPUT_STATE_BRANCH" ] && [ -n "$STATE_PATH" ]; then
    mkdir -p "$STATE_PATH"
    if git fetch "$REPO_URL" "$INPUT_STATE_BRANCH" 2>/dev/null; then
        git archive FETCH_HEAD | tar -x -C "$STATE_PATH" || true
    fi
fi

/sentinel "$@" || true

# Persist state back to branch after probing (skip on dry-run)
if [ -n "$INPUT_STATE_BRANCH" ] && [ -n "$STATE_PATH" ] && [ "$INPUT_DRY_RUN" != "true" ]; then
    git add "$STATE_PATH"
    if ! git diff --cached --quiet; then
        export GIT_AUTHOR_NAME="github-actions[bot]"
        export GIT_AUTHOR_EMAIL="github-actions[bot]@users.noreply.github.com"
        export GIT_COMMITTER_NAME="github-actions[bot]"
        export GIT_COMMITTER_EMAIL="github-actions[bot]@users.noreply.github.com"
        TREE=$(git write-tree --prefix="${STATE_PATH}/")
        PARENT=$(git ls-remote "$REPO_URL" "refs/heads/$INPUT_STATE_BRANCH" | awk '{print $1}')
        if [ -n "$PARENT" ]; then
            COMMIT=$(git commit-tree -m "chore: update sentinel state" -p "$PARENT" "$TREE")
        else
            COMMIT=$(git commit-tree -m "chore: update sentinel state" "$TREE")
        fi
        git push "$REPO_URL" "$COMMIT:refs/heads/$INPUT_STATE_BRANCH"
    fi
fi

exit 0