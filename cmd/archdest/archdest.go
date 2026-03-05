package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/hashicorp/go-set"
	"github.com/pkg/errors"
)

type Packages = *set.Set[string]

const etcArchdest = "/etc/archdest"

func pacmanList(arg string) (Packages, error) {
	out, err := exec.Command("pacman", arg).CombinedOutput()
	if err != nil {
		return nil, errors.WithStack(err)
	}
	var pkgs []string
	for pkg := range bytes.Lines(out) {
		pkgs = append(pkgs, string(bytes.TrimSpace(pkg)))
	}
	return set.From(pkgs), nil
}

func explicit() (Packages, error) {
	return pacmanList("-Qeq")
}

func all() (Packages, error) {
	return pacmanList("-Qq")
}

func pruneable() (Packages, error) {
	pkgs, err := pacmanList("-Qttdq")
	if err == nil {
		return pkgs, nil
	}
	return set.New[string](0), nil
}

func wanted() (Packages, error) {
	pkgs := set.New[string](50)
	err := filepath.Walk(etcArchdest, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		more, err := os.ReadFile(path)
		if err != nil {
			return errors.WithStack(err)
		}
		for pkg := range bytes.Lines(more) {
			if i := bytes.IndexByte(pkg, '#'); i != -1 {
				pkg = pkg[0:i]
			}
			pkg = bytes.TrimSpace(pkg)
			if len(pkg) == 0 {
				continue
			}
			pkgs.Insert(string(pkg))
		}
		return nil
	})
	return pkgs, errors.WithStack(err)
}

func pacman(args ...string) error {
	cmd := exec.Command("pacman", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return errors.WithStack(cmd.Run())
}

func markExplicit(pkgs Packages) error {
	return pacman(append([]string{"-D", "--asexplicit"}, pkgs.Slice()...)...)
}

func install(pkgs Packages) error {
	return pacman(append([]string{"-S"}, pkgs.Slice()...)...)
}

func markDeps(pkgs Packages) error {
	return pacman(append([]string{"-D", "--asdeps"}, pkgs.Slice()...)...)
}

func remove(pkgs Packages) error {
	return pacman(append([]string{"-Rns"}, pkgs.Slice()...)...)
}

func run() error {
	wantedPkgs, err := wanted()
	if err != nil {
		return err
	}

	explicitPkgs, err := explicit()
	if err != nil {
		return err
	}

	allPkgs, err := all()
	if err != nil {
		return err
	}

	missingOrNonExplicit := wantedPkgs.Difference(explicitPkgs)
	needsExplicit := missingOrNonExplicit.Intersect(allPkgs)
	if needsExplicit.Size() > 0 {
		if err := markExplicit(needsExplicit); err != nil {
			return err
		}
	}

	missing := missingOrNonExplicit.Difference(allPkgs)
	if missing.Size() > 0 {
		if err := install(missing); err != nil {
			return err
		}
	}

	unnecessaryExplicit := explicitPkgs.Difference(wantedPkgs)
	if unnecessaryExplicit.Size() > 0 {
		if err := markDeps(unnecessaryExplicit); err != nil {
			return err
		}
	}

	pruneablePkgs, err := pruneable()
	if err != nil {
		return err
	}

	if pruneablePkgs.Size() > 0 {
		if err := remove(pruneablePkgs); err != nil {
			return err
		}
	}

	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
}
