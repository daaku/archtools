// Command archdiff implements a tool to view and manipulate a "system
// level diff" of sorts. It's somewhat akin to the "things that differ"
// if a new system was given the exact current set of packages
// combined with a target directory that can be considered an "overlay"
// on top of the packages for things like configuration and or ignored
// data.
package main

import (
	"bufio"
	"crypto/md5"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/Jguer/go-alpm/v2"
	"github.com/gobwas/glob"
	"github.com/mattn/go-zglob/fastwalk"
	"github.com/pkg/errors"
)

type Glob interface {
	Match(name string) bool
}

type simpleGlob string

func (g simpleGlob) Match(path string) bool {
	if path == string(g) {
		return true
	}
	return strings.HasPrefix(path, string(g)) && len(g) > len(path) && g[len(path)] == '/'
}

func filehash(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.WithStack(err)
	}
	defer file.Close()
	h := md5.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", errors.WithStack(err)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func contains(a []string, x string) bool {
	i := sort.SearchStrings(a, x)
	if i == len(a) {
		return false
	}
	return a[i] == x
}

type App struct {
	Root       string
	DB         string
	Repo       string
	IgnoreDir  string
	CpuProfile string

	localDB alpm.IDB
	alpm    *alpm.Handle

	ignoreGlob         []Glob
	backupFile         map[string]string
	allFile            []string
	packageFile        []string
	repoFile           []string
	modifiedBackupFile []string
	unpackagedFile     []string
	modifiedRepoFile   []string
}

func (a *App) buildIgnoreGlob() error {
	var m sync.Mutex
	return errors.WithStack(fastwalk.FastWalk(
		a.IgnoreDir,
		func(path string, info os.FileMode) error {
			if info.IsDir() {
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return errors.WithStack(err)
			}
			defer f.Close()

			sc := bufio.NewScanner(f)
			for sc.Scan() {
				l := sc.Text()
				if len(l) == 0 {
					continue
				}
				if l[0] == '#' {
					continue
				}
				m.Lock()
				if strings.ContainsAny(l, "*?[") {
					g, err := glob.Compile(l)
					if err != nil {
						return errors.WithStack(err)
					}
					a.ignoreGlob = append(a.ignoreGlob, g)
				} else {
					a.ignoreGlob = append(a.ignoreGlob, simpleGlob(l))
				}
				m.Unlock()
			}
			return errors.WithStack(sc.Err())
		},
	))
}

func (a *App) isIgnored(path string) bool {
	for _, glob := range a.ignoreGlob {
		if glob.Match(path) {
			return true
		}
	}
	return false
}

func (a *App) initAlpm() error {
	var err error
	a.alpm, err = alpm.Initialize(a.Root, a.DB)
	if err != nil {
		return errors.WithStack(err)
	}
	a.localDB, err = a.alpm.LocalDB()
	return errors.WithStack(err)
}

func (a *App) buildAllFile() error {
	var m sync.Mutex
	return errors.WithStack(fastwalk.FastWalk(
		a.Root,
		func(path string, info os.FileMode) error {
			// an artifact of fastwalk.FastWalk somehow
			if strings.HasPrefix(path, "//") {
				path = path[1:]
			}
			if a.isIgnored(path) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if info.IsDir() {
				return nil
			}
			m.Lock()
			a.allFile = append(a.allFile, path)
			m.Unlock()
			return nil
		}))
}

func (a *App) buildPackageFile() error {
	err := a.localDB.PkgCache().ForEach(func(pkg alpm.IPackage) error {
		for _, file := range pkg.Files() {
			a.packageFile = append(a.packageFile, filepath.Join("/", file.Name))
		}
		return nil
	})
	sort.Strings(a.packageFile)
	return errors.WithStack(err)
}

func (a *App) buildBackupFile() error {
	a.backupFile = make(map[string]string)
	return errors.WithStack(
		a.localDB.PkgCache().ForEach(func(pkg alpm.IPackage) error {
			return pkg.Backup().ForEach(func(bf alpm.BackupFile) error {
				a.backupFile[filepath.Join("/", bf.Name)] = bf.Hash
				return nil
			})
		}))
}

func (a *App) buildRepoFile() error {
	var m sync.Mutex
	err := fastwalk.FastWalk(a.Repo,
		func(path string, info os.FileMode) error {
			if info.IsDir() {
				return nil
			}
			name := strings.Replace(path, a.Repo, "", 1)
			m.Lock()
			a.repoFile = append(a.repoFile, name)
			m.Unlock()
			return nil
		})
	sort.Strings(a.repoFile)
	return errors.WithStack(err)
}

func (a *App) buildUnpackagedFile() error {
	for _, file := range a.allFile {
		if !contains(a.packageFile, file) {
			a.unpackagedFile = append(a.unpackagedFile, file)
		}
	}
	return nil
}

func (a *App) buildModifiedBackupFile() error {
	for file, hash := range a.backupFile {
		if contains(a.repoFile, file) {
			continue
		}
		fullname := filepath.Join(a.Root, file)
		if a.isIgnored(fullname) {
			continue
		}
		actual, err := filehash(fullname)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return errors.WithStack(err)
		}
		if actual != hash {
			a.modifiedBackupFile = append(a.modifiedBackupFile, file)
		}
	}
	return nil
}

func (a *App) buildModifiedRepoFile() error {
	for _, file := range a.repoFile {
		realpath := filepath.Join(a.Root, file)
		repopath := filepath.Join(a.Repo, file)
		realhash, err := filehash(realpath)
		if err != nil && !os.IsNotExist(err) {
			return errors.WithStack(err)
		}
		repohash, err := filehash(repopath)
		if err != nil && !os.IsNotExist(err) {
			return errors.WithStack(err)
		}
		if realhash != repohash {
			a.modifiedRepoFile = append(a.modifiedRepoFile, file)
		}
	}
	return nil
}

func Main() error {
	var app App
	flag.StringVar(&app.Root, "root", "/", "set an alternate installation root")
	flag.StringVar(
		&app.DB, "dbpath", "/var/lib/pacman", "set an alternate database location")
	flag.StringVar(&app.Repo, "repo", "/usr/share/archdiff", "repo directory")
	flag.StringVar(&app.IgnoreDir, "ignore", "/etc/archdiff/ignore",
		"directory of ignore files")
	flag.StringVar(&app.CpuProfile, "cpuprofile", "", "write cpu profile here")
	flag.Parse()

	if app.CpuProfile != "" {
		f, err := os.Create(app.CpuProfile)
		if err != nil {
			return errors.WithStack(err)
		}
		defer f.Close()
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	steps := []func() error{
		app.initAlpm,
		app.buildIgnoreGlob,
		app.buildAllFile,
		app.buildPackageFile,
		app.buildBackupFile,
		app.buildRepoFile,
		app.buildUnpackagedFile,
		app.buildModifiedBackupFile,
		app.buildModifiedRepoFile,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}

	diff := slices.Concat(app.unpackagedFile, app.modifiedRepoFile, app.modifiedBackupFile)
	sort.Strings(diff)

	for _, file := range diff {
		fmt.Println(file)
	}

	return nil
}

func main() {
	if err := Main(); err != nil {
		fmt.Fprintf(os.Stderr, "%+v", err)
		os.Exit(1)
	}
}
