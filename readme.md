# archdiff

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
