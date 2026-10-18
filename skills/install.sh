#!/bin/sh
# Install the parley skills where the harnesses look for skills: ~/.claude/skills
# for Claude Code, ~/.agents/skills for the agentskills.io convention Codex
# reads. There are three, parley-build, parley-table and parley-line, and each is
# a symlink into this checkout, so the skill a session loads is the one in the
# repo and an edit to it needs no reinstall.
#
# Idempotent: a link that already points here is left alone and reported. A path
# that is anything else is never replaced — the script says what it found and
# leaves it, because the thing in the way is somebody's, and not this script's to
# throw away. Installing examines every destination before it touches any, so a
# refusal over a later one does not leave an earlier one already changed; and if
# linking fails anyway after that — a destination that cannot even be made — the
# links this run had already created are removed again, so a run installs all of
# them or leaves none. A link is only removed again while it is still a link to
# this checkout: a path somebody has put there since is theirs, and is left.
#
# Installing also clears away the old name. Before parley-build the two-session
# skill was linked as parley, and a link of that name under either skills root
# that points into this checkout is removed once everything else is in place: it
# is dangling by now, and would offer a session a skill that is gone. Anything
# else at that name — a directory, a link to somewhere else — is not ours; the
# script names it in one warning and leaves it.
#
# Uninstalling is not all-or-nothing, because there is nothing to keep in step:
# every link that is ours is removed — a symlink still resolving to this
# checkout's skill directory, and nothing else — each foreign path is reported
# and left alone, and only the exit status waits for the end — 1 if any path was
# foreign.
# A stranger's directory at one destination is no reason to leave our link at
# another one installed.
set -eu

usage() {
	echo "usage: skills/install.sh [--uninstall]" >&2
}

mode=install
if [ "$#" -gt 1 ]; then
	echo "install.sh: too many arguments" >&2
	usage
	exit 1
fi
case "${1-}" in
'') ;;
--uninstall) mode=uninstall ;;
-h | --help)
	usage
	exit 0
	;;
*)
	echo "install.sh: unknown argument: $1" >&2
	usage
	exit 1
	;;
esac

# The skills to link are beside this script, whatever directory the script was
# invoked from and whatever the caller's cwd is. -P resolves the path the same
# way the comparison below resolves an existing link, so a checkout reached
# through a symlink compares equal to itself. CDPATH is unset in every
# subshell that runs cd: a cd that lands through CDPATH prints where it went,
# and that line would be captured ahead of pwd's and corrupt the path.
here=$(unset CDPATH; cd -P "$(dirname "$0")" && pwd)
skill_names="parley-build parley-table parley-line"
stale_name=parley
for name in $skill_names; do
	if [ ! -d "$here/$name" ]; then
		echo "install.sh: $here/$name is not a directory; run this script from a parley checkout" >&2
		exit 1
	fi
done

# linked_here reports whether dest ($1) is a symlink resolving to source ($2). A
# dangling link resolves to nothing and is not ours.
linked_here() {
	[ -L "$1" ] || return 1
	resolved=$(unset CDPATH; cd -P -- "$1" 2>/dev/null && pwd) || return 1
	[ "$resolved" = "$2" ]
}

# check_one is the install preflight for one destination, the skill $2 under the
# skills root $1: absent, or a link that already points here, is workable;
# anything else is the refusal, and refusing before any link is made is what
# keeps the destinations in step.
check_one() {
	dest=$1/$2
	source_dir=$here/$2
	if linked_here "$dest" "$source_dir"; then
		return 0
	fi
	if [ -e "$dest" ] || [ -L "$dest" ]; then
		echo "install.sh: $dest exists and is not a link to $source_dir; leaving it alone" >&2
		exit 1
	fi
	return 0
}

# link_one links the skill $2 under the skills root $1 and answers 1 if it could
# not. linked_now says which of the two ways it succeeded: 1 when this run
# created the link, 0 when the link was already there. The rollback below keys
# on it, because a link this run did not make is not this run's to remove.
link_one() {
	root=$1
	dest=$root/$2
	source_dir=$here/$2
	linked_now=0
	if linked_here "$dest" "$source_dir"; then
		echo "already linked: $dest -> $source_dir"
		return 0
	fi
	mkdir -p -- "$root" || return 1
	ln -s -- "$source_dir" "$dest" || return 1
	linked_now=1
	echo "linked: $dest -> $source_dir"
}

# unlink_one removes the skill $2 under the skills root $1 if it is ours, and
# answers 1 if the path is somebody else's — reported, left alone, and not the
# end of the run: the caller goes on to the other destinations and lets the
# status fall out at the end.
unlink_one() {
	dest=$1/$2
	source_dir=$here/$2
	if linked_here "$dest" "$source_dir"; then
		rm -- "$dest"
		echo "removed: $dest"
		return 0
	fi
	if [ -e "$dest" ] || [ -L "$dest" ]; then
		echo "install.sh: $dest is not a link to $source_dir; leaving it alone" >&2
		return 1
	fi
	echo "not present: $dest"
	return 0
}

# stale_link_here reports whether dest ($1) is a symlink whose target lies inside
# this checkout's skills directory. The stale link's target no longer exists, so
# linked_here's cd -P through the link cannot resolve it; the target's directory
# can be resolved, which is what says whose checkout the link was made from. A
# relative target is taken from the link's own directory, and a target ending
# in . or .. names its directory's parent rather than anything inside it, so it
# is not ours.
stale_link_here() {
	[ -L "$1" ] || return 1
	target=$(readlink -- "$1") || return 1
	case "$target" in
	/*) ;;
	*) target=$(dirname -- "$1")/$target ;;
	esac
	while [ "${target%/}" != "$target" ] && [ "$target" != / ]; do
		target=${target%/}
	done
	case "${target##*/}" in
	'' | . | ..) return 1 ;;
	esac
	parent=$(unset CDPATH; cd -P -- "$(dirname -- "$target")" 2>/dev/null && pwd) || return 1
	case "$parent/" in
	"$here"/*) return 0 ;;
	esac
	return 1
}

# remove_stale_one clears the old-named skill under the skills root $1 if it is
# a link into this checkout, and says so; anything else at that name is left
# with one warning line. Neither is a failure of the install, which is complete
# by the time this runs.
remove_stale_one() {
	dest=$1/$stale_name
	if stale_link_here "$dest"; then
		if rm -- "$dest"; then
			echo "removed stale: $dest"
		else
			echo "install.sh: could not remove the stale $dest" >&2
		fi
		return 0
	fi
	if [ -e "$dest" ] || [ -L "$dest" ]; then
		echo "install.sh: $dest is not a link into $here; leaving it alone" >&2
	fi
	return 0
}

claude_skills=$HOME/.claude/skills
agent_skills=$HOME/.agents/skills

if [ "$mode" = uninstall ]; then
	status=0
	for root in "$claude_skills" "$agent_skills"; do
		for name in $skill_names; do
			unlink_one "$root" "$name" || status=1
		done
	done
	exit "$status"
fi

for root in "$claude_skills" "$agent_skills"; do
	for name in $skill_names; do
		check_one "$root" "$name"
	done
done

# The preflight cleared every destination, so what can still fail here is making
# a destination directory itself — ~/.agents a regular file, a mode that forbids
# it. Earlier links are already made by then, and half an install is worse than
# none: the Codex front would find no skill while Claude Code found one, and
# nothing would say why. So the run takes its own links back out and says so.
# The positional parameters hold the links this run made, in the order it made
# them; the arguments were parsed above and are not needed again. Each is taken
# out only if it is still a link to this checkout's skill directory, as at
# uninstall: the run made it, but by now it may be a path somebody else owns.
set --
for root in "$claude_skills" "$agent_skills"; do
	for name in $skill_names; do
		if link_one "$root" "$name"; then ok=1; else ok=0; fi
		# A link made before link_one failed (its report could not be written)
		# is this run's too, so it joins the set before any rollback reads it.
		if [ "$linked_now" = 1 ]; then
			set -- "$@" "$root/$name"
		fi
		if [ "$ok" = 0 ]; then
			echo "install.sh: could not link $root/$name" >&2
			left=0
			for made in "$@"; do
				if linked_here "$made" "$here/${made##*/}"; then
					rm -- "$made"
					echo "install.sh: removed $made, a link this run had just made" >&2
				else
					echo "install.sh: $made is no longer a link to $here/${made##*/}; leaving it alone" >&2
					left=1
				fi
			done
			if [ "$#" -gt 0 ] && [ "$left" = 0 ]; then
				echo "install.sh: nothing from this run is left behind" >&2
			fi
			exit 1
		fi
	done
done
# Every link is in place, so the old name can go without leaving an install
# that is missing a skill.
for root in "$claude_skills" "$agent_skills"; do
	remove_stale_one "$root"
done
echo "the skills are /parley-build, /parley-table and /parley-line in Claude Code and \$parley-build, \$parley-table and \$parley-line in Codex"
