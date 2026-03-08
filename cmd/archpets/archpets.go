package main

import (
	"bytes"
	"context"
	serrors "errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/daaku/prefixer"
	"github.com/jpillora/opts"
	"github.com/pelletier/go-toml/v2"
	"github.com/pkg/errors"
	"github.com/pkg/sftp/v2"
	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

var currentUser, _ = user.Current()

type FileMeta struct {
	UID  uint32      `toml:"uid"`
	GID  uint32      `toml:"gid"`
	Mode fs.FileMode `toml:"mode"`
}

func (m *FileMeta) diffOwner(stat fs.FileInfo) (bool, error) {
	attr, ok := stat.Sys().(*sshfx.Attributes)
	if !ok {
		return false, errors.Errorf("expected %T, but got %T for %s", attr, stat.Sys(), stat.Name())
	}
	return attr.UID != m.UID || attr.GID != m.GID, nil
}

func (m *FileMeta) diffAll(stat fs.FileInfo) (bool, error) {
	diff, err := m.diffOwner(stat)
	if err != nil {
		return false, err
	}
	return diff || stat.Mode().Perm() != m.Mode, nil
}

type SourceConfig struct {
	Files map[string]FileMeta `toml:"files"`
}

func (c *SourceConfig) FileMeta(filename string, dir bool) (FileMeta, error) {
	mode := fs.FileMode(0o644)
	if dir {
		mode = 0o755
	}
	for pattern, meta := range c.Files {
		match, err := doublestar.Match(pattern, filename)
		if err != nil {
			return FileMeta{}, errors.WithStack(err)
		}
		if match {
			if meta.Mode == 0 {
				meta.Mode = mode
			}
			return meta, nil
		}
	}
	// defaults
	return FileMeta{Mode: mode}, nil
}

type File interface {
	String() string
	DestPath() string
	Run(client *sftp.Client) error
	IsDiff(client *sftp.Client) (bool, error)
}

func atomicallyReplace(
	client *sftp.Client,
	filename string,
	meta FileMeta,
	src io.Reader,
) error {
	open := func() (string, *sftp.File, error) {
		for i := range 10 {
			name := fmt.Sprintf("%s.pets.%d", filename, i)
			f, err := client.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_EXCL, meta.Mode)
			if err == nil {
				return name, f, nil
			}
		}
		return "", nil, errors.Errorf("giving up creating temp file after 10 tries for %q", filename)
	}

	tempPath, dst, err := open()
	if err != nil {
		return err
	}

	if err := client.Chown(tempPath, int(meta.UID), int(meta.GID)); err != nil {
		dst.Close()
		client.Remove(tempPath)
		return errors.WithStack(err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		client.Remove(tempPath)
		return errors.WithStack(err)
	}

	if err := dst.Close(); err != nil {
		client.Remove(tempPath)
		return errors.WithStack(err)
	}

	if err := client.Rename(tempPath, filename); err != nil {
		client.Remove(tempPath)
		return errors.WithStack(err)
	}

	return nil
}

type FileMkdir struct {
	destPath string
	meta     FileMeta
}

func (t *FileMkdir) String() string {
	return fmt.Sprintf("M %s", t.destPath)
}

func (t *FileMkdir) DestPath() string {
	return t.destPath
}

func (t *FileMkdir) Run(client *sftp.Client) error {
	stat, err := client.Stat(t.destPath)
	if err != nil {
		if !serrors.Is(err, fs.ErrNotExist) {
			return errors.WithStack(err)
		}
		if err := client.Mkdir(t.destPath, t.meta.Mode); err != nil {
			return errors.WithStack(err)
		}
	} else {
		if !stat.IsDir() {
			return errors.Errorf("unable to replace non-directory with directory: %s", t.destPath)
		}
		if stat.Mode() != t.meta.Mode {
			if err := client.Chmod(t.destPath, t.meta.Mode); err != nil {
				return errors.WithStack(err)
			}
		}
	}
	if err := client.Chown(t.destPath, int(t.meta.UID), int(t.meta.GID)); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (t *FileMkdir) IsDiff(client *sftp.Client) (bool, error) {
	stat, err := client.Stat(t.destPath)
	if err != nil {
		if serrors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		return false, errors.WithStack(err)
	}
	if !stat.IsDir() {
		return false, errors.Errorf("unable to replace file with directory: %s", t.destPath)
	}
	return t.meta.diffAll(stat)
}

type FileCopy struct {
	destPath   string
	sourcePath string
	meta       FileMeta
}

func (t *FileCopy) String() string {
	return fmt.Sprintf("F %s from %s", t.destPath, t.sourcePath)
}

func (t *FileCopy) DestPath() string {
	return t.destPath
}

func (t *FileCopy) Run(client *sftp.Client) error {
	src, err := os.Open(t.sourcePath)
	if err != nil {
		return errors.WithStack(err)
	}
	defer src.Close()
	return atomicallyReplace(client, t.destPath, t.meta, src)
}

func (t *FileCopy) IsDiff(client *sftp.Client) (bool, error) {
	src, err := os.ReadFile(t.sourcePath)
	if err != nil {
		return false, errors.WithStack(err)
	}

	dstStat, err := client.Stat(t.destPath)
	if err != nil {
		if serrors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		return false, errors.WithStack(err)
	}
	diff, err := t.meta.diffAll(dstStat)
	if err != nil {
		return false, errors.WithStack(err)
	}
	if diff {
		return true, nil
	}

	dstF, err := client.Open(t.destPath)
	if err != nil {
		return false, errors.WithStack(err)
	}

	dst, err := io.ReadAll(dstF)
	if err != nil {
		return false, errors.WithStack(err)
	}

	return !bytes.Equal(src, dst), nil
}

type FileSymlink struct {
	destPath   string
	targetPath string
	meta       FileMeta
}

func (t *FileSymlink) String() string {
	return fmt.Sprintf("L %s to %s", t.destPath, t.targetPath)
}

func (t *FileSymlink) DestPath() string {
	return t.destPath
}

func (t *FileSymlink) IsDiff(client *sftp.Client) (bool, error) {
	lstat, err := client.LStat(t.destPath)
	if err != nil {
		if serrors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		return false, errors.WithStack(err)
	}
	if lstat.Mode().IsDir() || lstat.Mode().IsRegular() {
		return true, nil
	}
	diff, err := t.meta.diffOwner(lstat)
	if err != nil {
		return true, errors.WithStack(err)
	}
	if diff {
		return true, nil
	}
	current, err := client.ReadLink(t.destPath)
	if err != nil {
		if serrors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		return false, errors.WithStack(err)
	}
	return current != t.targetPath, nil
}

func (t *FileSymlink) Run(client *sftp.Client) error {
	client.Remove(t.destPath) // ignore error
	if err := client.Symlink(t.targetPath, t.destPath); err != nil {
		return errors.WithStack(err)
	}
	if err := client.Chown(t.targetPath, int(t.meta.UID), int(t.meta.GID)); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

type mutSlice[T any] struct {
	s []T
}

func (m *mutSlice[T]) append(t T) {
	m.s = append(m.s, t)
}

type System struct {
	Name         string
	Source       string
	Host         string `toml:"host"`
	Port         int    `toml:"port"`
	Root         string
	SourceConfig *SourceConfig
}

func (s *System) FileMeta(destPath string, dir bool) (FileMeta, error) {
	relPath, err := filepath.Rel(s.Root+"/", destPath)
	if err != nil {
		return FileMeta{}, errors.WithStack(err)
	}
	return s.SourceConfig.FileMeta(relPath, dir)
}

func (s *System) walkFunc(files *mutSlice[File], path, destPath string, d fs.DirEntry, err error) error {
	if err != nil {
		return errors.WithStack(err)
	}

	meta, err := s.FileMeta(destPath, d.IsDir())
	if err != nil {
		return err
	}

	if d.IsDir() {
		if destPath != "/" {
			files.append(&FileMkdir{
				destPath: destPath,
				meta:     meta,
			})
		}
		return nil
	}

	if d.Type()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return errors.WithStack(err)
		}

		// abs path symlink, keep it
		if filepath.IsAbs(target) {
			files.append(&FileSymlink{
				destPath:   destPath,
				targetPath: target,
				meta:       meta,
			})
			return nil
		}

		cleanTarget := filepath.Join(filepath.Dir(path), target)

		// in-tree symlink, keep it
		if strings.HasPrefix(cleanTarget, s.Source) {
			files.append(&FileSymlink{
				destPath:   destPath,
				targetPath: target,
				meta:       meta,
			})
			return nil
		}

		targetInfo, err := os.Lstat(cleanTarget)
		if err != nil {
			return errors.WithStack(err)
		}

		if targetInfo.Mode()&fs.ModeSymlink != 0 {
			return errors.Errorf("symlink to symlink is unsupported: %s", path)
		}

		absTarget, err := filepath.Abs(cleanTarget)
		if err != nil {
			return errors.WithStack(err)
		}

		if targetInfo.IsDir() {
			err := filepath.WalkDir(absTarget, func(path string, d fs.DirEntry, err error) error {
				newDestPath := strings.Replace(path, absTarget, destPath, 1)
				return s.walkFunc(files, path, newDestPath, d, err)
			})
			if err != nil {
				return err
			}
			return nil
		}

		files.append(&FileCopy{
			sourcePath: absTarget,
			destPath:   destPath,
			meta:       meta,
		})

		return nil
	}

	// normal file
	files.append(&FileCopy{
		sourcePath: path,
		destPath:   destPath,
		meta:       meta,
	})
	return nil
}

func (s *System) Files() ([]File, error) {
	configPath := filepath.Join(s.Source, "config.toml")
	var files mutSlice[File]
	err := filepath.WalkDir(s.Source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return errors.WithStack(err)
		}
		if path == configPath { // ignore config.toml
			return nil
		}
		destPath, err := filepath.Rel(s.Source, path)
		if err != nil {
			return errors.WithStack(err)
		}
		destPath = filepath.Join(s.Root, destPath)
		return s.walkFunc(&files, path, destPath, d, err)
	})
	if err != nil {
		return nil, err
	}
	return files.s, nil
}

type App struct {
	Repo    string   `opts:"name=repo,short=g,env=PETS_REPO,help=repo of systems"`
	Root    string   `opts:"name=root,short=r,help=alternate root directory"`
	Cmd     string   `opts:"mode=arg"`
	Names   []string `opts:"mode=arg,help=No names implies all systems."`
	Verbose bool     `opts:"name=verbose,short=v,help=verbose output"`
	DryRun  bool     `opts:"name=dry-run,short=n,help=dry run mode"`

	SourceConfig SourceConfig `opts:"-"`
}

func (a *App) Init(ctx context.Context) error {
	if len(a.Names) == 0 {
		var err error
		a.Names, err = a.Discover()
		if err != nil {
			return err
		}
	}

	configPath := filepath.Join(a.Repo, "pets.toml")
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		if !serrors.Is(err, fs.ErrNotExist) {
			return errors.Errorf("error reading config %q: %v", configPath, err)
		}
	} else {
		err = toml.NewDecoder(bytes.NewReader(configBytes)).
			DisallowUnknownFields().
			Decode(&a.SourceConfig)
		if err != nil {
			return errors.Errorf("error decoding config %q: %v", configPath, err)
		}
	}

	return nil
}

func (a *App) Named(systemName string) (System, error) {
	source := filepath.Join(a.Repo, "system", systemName)
	configPath := filepath.Join(source, "config.toml")
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		return System{}, errors.Errorf("error reading config %q: %v", configPath, err)
	}
	s := System{
		Name:         systemName,
		Root:         a.Root,
		Port:         22,
		SourceConfig: &a.SourceConfig,
	}
	err = toml.NewDecoder(bytes.NewReader(configBytes)).
		DisallowUnknownFields().
		Decode(&s)
	if err != nil {
		return System{}, errors.Errorf("error decoding config %q: %v", configPath, err)
	}
	s.Source = source
	return s, nil
}

func (a *App) Discover() ([]string, error) {
	dir := filepath.Join(a.Repo, "system")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

func (a *App) CmdLs(ctx context.Context, s System, stdout io.Writer) error {
	files, err := s.Files()
	if err != nil {
		return err
	}
	for _, f := range files {
		fmt.Fprintln(stdout, f)
	}
	return nil
}

func (a *App) diffOrDeploy(ctx context.Context, s System, stdout io.Writer, dryRun bool) error {
	client, err := connectToHost(s)
	if err != nil {
		return err
	}
	defer client.Close()
	sftpClient, err := sftp.NewClient(ctx, client)
	if err != nil {
		return errors.WithStack(err)
	}
	defer sftpClient.Close()
	files, err := s.Files()
	if err != nil {
		return err
	}
	var ignore bytes.Buffer
	for _, f := range files {
		ignorePath := f.DestPath()
		if a.Root != "/" {
			ignorePath = strings.TrimPrefix(ignorePath, a.Root)
		}
		fmt.Fprintln(&ignore, ignorePath)

		diff, err := f.IsDiff(sftpClient)
		if err != nil {
			return err
		}
		if !diff {
			if a.Verbose {
				fmt.Fprintf(stdout, "unchanged: %s\n", f.String())
			}
			continue
		}
		fmt.Fprintf(stdout, "changed: %s\n", f.String())
		if !dryRun {
			if err := f.Run(sftpClient); err != nil {
				return err
			}
		}
	}

	if !dryRun {
		// FIXME only update if changed
		petsIgnoreFilename := filepath.Join(a.Root, "/etc/archdiff/ignore/pets")
		fmt.Fprintln(&ignore, petsIgnoreFilename)
		petsMeta := FileMeta{Mode: fs.FileMode(0o644)}
		if err := atomicallyReplace(sftpClient, petsIgnoreFilename, petsMeta, &ignore); err != nil {
			return err
		}
	}

	return nil
}

func (a *App) CmdDeploy(ctx context.Context, s System, stdout io.Writer) error {
	return a.diffOrDeploy(ctx, s, stdout, false)
}

func (a *App) CmdDiff(ctx context.Context, s System, stdout io.Writer) error {
	return a.diffOrDeploy(ctx, s, stdout, true)
}

func getDefaultSigners() ([]ssh.Signer, error) {
	defaults := []string{
		filepath.Join(currentUser.HomeDir, ".ssh", "id_ed25519"),
		filepath.Join(currentUser.HomeDir, ".ssh", "id_rsa"),
		filepath.Join(currentUser.HomeDir, ".ssh", "id_ecdsa"),
		// FIXME: custom identity file option
		filepath.Join(currentUser.HomeDir, ".ssh", "aws_ap_south_1_rsa"),
	}
	var signers []ssh.Signer
	for _, f := range defaults {
		if _, err := os.Stat(f); err == nil {
			key, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			signer, err := ssh.ParsePrivateKey(key)
			if err != nil {
				continue
			}
			signers = append(signers, signer)
		}
	}
	return signers, nil
}

func getSSHAuthMethods() []ssh.AuthMethod {
	methods := []ssh.AuthMethod{ssh.PublicKeysCallback(getDefaultSigners)}
	if agentSock := os.Getenv("SSH_AUTH_SOCK"); agentSock != "" {
		conn, err := net.Dial("unix", agentSock)
		if err == nil {
			agentClient := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(agentClient.Signers))
		}
	}
	return methods
}

func connectToHost(s System) (*ssh.Client, error) {
	config := &ssh.ClientConfig{
		User:            "root",
		Auth:            getSSHAuthMethods(),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}

	addr := net.JoinHostPort(s.Host, fmt.Sprint(s.Port))
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return client, nil
}

var cmds = map[string]func(*App, context.Context, System, io.Writer) error{
	"ls":     (*App).CmdLs,
	"diff":   (*App).CmdDiff,
	"deploy": (*App).CmdDeploy,
}

var errSentinel = errors.New("sentinel error")

func run(ctx context.Context) error {
	a := App{
		Root: "/",
		Repo: "/home/naitik/workspace/pets",
	}
	opts.Parse(&a)

	cmd, found := cmds[a.Cmd]
	if !found {
		return errors.Errorf("unknown command: %s", a.Cmd)
	}

	if err := a.Init(ctx); err != nil {
		return err
	}

	var outMu sync.Mutex
	var hasErr atomic.Bool
	var wg sync.WaitGroup
	for _, name := range a.Names {
		wg.Go(func() {
			var prefix []byte
			if len(a.Names) > 1 {
				prefix = fmt.Appendf(nil, "%s: ", name)
			}
			stdout := new(bytes.Buffer)
			s, err := a.Named(name)
			if err == nil {
				err = cmd(&a, ctx, s, stdout)
			}
			outMu.Lock()
			defer outMu.Unlock()
			io.Copy(prefixer.New(os.Stdout, prefix), stdout)
			if err != nil {
				hasErr.Store(true)
				fmt.Fprintf(prefixer.New(os.Stderr, prefix), "%+v\n", err)
			}
		})
	}
	wg.Wait()
	if hasErr.Load() {
		return errSentinel
	}
	return nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		if err != errSentinel {
			fmt.Fprintf(os.Stderr, "%+v\n", err)
		}
		os.Exit(1)
	}
}
