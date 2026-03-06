# archtools

archtools provides a set of tools that enables managing [Arch Linux](arch)
machines. It's probably more suited for _pets_ rather than _cattle_. If you have
multiple workstations, several named servers or something along those lines,
and would like to be able to share configuration, package sets and manage the
evolution of those, you may find these tools useful. Alternatives like `NixOS`
exist and possibly immutable distros may be the future, but `Arch` has served me
well for a very long time and I still enjoy it.

## How does it work?

- `archdiff` helps you identify untracked changes in your system. This is meant
  to help you find configuration files that need to be tracked.
- `archdest` helps you manage installed packages. This is meant to ensure the
  packages you defined, and _only_ those pakages are installed.
- `archpets` helps you use a package repository to share configuration files
  across your systems.

## FAQs

1. How do you bootstrap a new system?
1. How do you migrate an existing system to use this?

## archdiff

ArchDiff provides a way to see a "diff" for your entire [Arch
Linux][arch] system. This includes showing modified config files &
unpackaged files.

It looks at a "shadow" tree to check if unpackaged files or modified config
files are already being tracked outside of pacman. This allows for cleanly
ignoring modified config files.

[arch]: http://www.archlinux.org/

## archpets

`pets` provides an opinionated tool to manage configuration files.

### todo

- local: status using sha256 only
- local: diff with actual content

- works locally
- works over ssh
- alternate root directory / chroot

- pull changes to repo
- system package manifest
- dry run

### overlay fs

- real file: sync file
- relative invalid symlink: error
- relative valid symlink: sync target as file
- absolute symlink: sync as symlink
