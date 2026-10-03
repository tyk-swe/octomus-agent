#!/bin/sh
# Redirect only fixture GitHub identity to the local bare remote. All Git operations are real.
if [ -n "${OCTOMUS_TOKEN+set}${OCTOMUS_NOTIFICATION_WEBHOOK_URL+set}" ]; then
    echo 'fixture git: a service secret reached the Git environment' >&2
    exit 1
fi
url=https://github.com/fixture/project.git
if [ "$#" -eq 3 ] && [ "$1" = remote ] && [ "$2" = get-url ] && [ "$3" = origin ]; then
    echo "$url"
    exit 0
fi
for arg; do
    shift
    if [ "$arg" = "$url" ]; then
        arg=$OCTOMUS_FIXTURE/remote.git
    fi
    set -- "$@" "$arg"
done
exec /usr/bin/git "$@"
