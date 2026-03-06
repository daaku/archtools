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

	"github.com/daaku/prefixer"
	"github.com/jpillora/opts"
	"github.com/pelletier/go-toml/v2"
	"github.com/pkg/errors"
	"github.com/pkg/sftp/v2"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

var currentUser, _ = user.Current()

type File interface {
	String() string
	DestPath() string
	Run(client *sftp.Client) error
	IsDiff(client *sftp.Client) (bool, error)
}

type FileCopy struct {
	destPath   string
	sourcePath string
	perms      fs.FileMode
}

func (t *FileCopy) String() string {
	return fmt.Sprintf("F %s from %s", t.destPath, t.sourcePath)
}

func (t *FileCopy) DestPath() string {
	return t.destPath
}

func (t *FileCopy) openTemp(client *sftp.Client) (string, *sftp.File, error) {
	// FIXME perms
	err := client.MkdirAll(filepath.Dir(t.destPath), 0o755)
	if err != nil {
		return "", nil, errors.WithStack(err)
	}
	for i := range 10 {
		name := fmt.Sprintf("%s.pets.%d", t.destPath, i)
		f, err := client.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_EXCL, t.perms)
		if err == nil {
			return name, f, nil
		}
	}
	return "", nil, errors.Errorf("giving up creating temp file after 10 tries for %q", t.destPath)
}

func (t *FileCopy) Run(client *sftp.Client) error {
	tempPath, dst, err := t.openTemp(client)
	if err != nil {
		return err
	}

	src, err := os.Open(t.sourcePath)
	if err != nil {
		dst.Close()
		client.Remove(tempPath)
		return errors.WithStack(err)
	}
	defer src.Close()

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		client.Remove(tempPath)
		return errors.WithStack(err)
	}

	if err := dst.Close(); err != nil {
		client.Remove(tempPath)
		return errors.WithStack(err)
	}

	if err := client.Rename(tempPath, t.destPath); err != nil {
		client.Remove(tempPath)
		return errors.WithStack(err)
	}

	return nil
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
	if dstStat.Mode() != t.perms {
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
}

func (t *FileSymlink) String() string {
	return fmt.Sprintf("L %s to %s", t.destPath, t.targetPath)
}

func (t *FileSymlink) DestPath() string {
	return t.destPath
}

func (t *FileSymlink) IsDiff(client *sftp.Client) (bool, error) {
	current, err := client.ReadLink(t.destPath)
	if err != nil {
		return false, errors.WithStack(err)
	}
	return current != t.targetPath, nil
}

func (t *FileSymlink) Run(client *sftp.Client) error {
	client.Remove(t.destPath) // ignore error
	if err := client.Symlink(t.targetPath, t.destPath); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

type System struct {
	Name   string
	Source string
	Host   string `toml:"host"`
	Root   string
}

type mutSlice[T any] struct {
	s []T
}

func (m *mutSlice[T]) append(t T) {
	m.s = append(m.s, t)
}

func (s *System) walkFunc(files *mutSlice[File], path, destPath string, d fs.DirEntry, err error) error {
	if err != nil {
		return errors.WithStack(err)
	}
	if d.IsDir() { // continue on directories
		// FIXME: handle empty directories?
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
			})
			return nil
		}

		cleanTarget := filepath.Join(filepath.Dir(path), target)

		// in-tree symlink, keep it
		if strings.HasPrefix(cleanTarget, s.Source) {
			files.append(&FileSymlink{
				destPath:   destPath,
				targetPath: target,
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
			perms:      0o644, // FIXME
		})

		return nil
	}

	// normal file
	files.append(&FileCopy{
		sourcePath: path,
		destPath:   destPath,
		perms:      0o644, // FIXME
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
	Repo   string   `opts:"name=repo,short=g,env=PETS_REPO,help=repo of systems"`
	Root   string   `opts:"name=root,short=r,help=alternate root directory"`
	Cmd    string   `opts:"mode=arg"`
	Names  []string `opts:"mode=arg,help=No names implies all systems."`
	DryRun bool     `opts:"name=dry-run,short=n,help=dry run mode"`
}

func (a *App) Named(systemName string) (System, error) {
	source := filepath.Join(a.Repo, "system", systemName)
	configPath := filepath.Join(source, "config.toml")
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		return System{}, errors.Errorf("error reading config %q: %v", configPath, err)
	}
	s := System{
		Name: systemName,
		Root: a.Root,
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

func (a *App) CmdDeploy(ctx context.Context, s System, stdout io.Writer) error {
	client, err := connectToHost(s.Host)
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
			fmt.Fprintf(stdout, "unchanged: %s\n", f.String())
			continue
		}
		fmt.Fprintf(stdout, "changed: %s\n", f.String())
		if err := f.Run(sftpClient); err != nil {
			return err
		}
	}

	// FIXME only update if changed
	// FIXME atomically write like FileCopy
	archdiffDir := filepath.Join(a.Root, "/etc/archdiff/ignore")
	if err := sftpClient.MkdirAll(archdiffDir, 0o755); err != nil {
		return errors.WithStack(err)
	}
	petsFileName := filepath.Join(archdiffDir, "pets")
	fmt.Fprintln(&ignore, petsFileName)
	petsFile, err := sftpClient.Create(petsFileName)
	if err != nil {
		return errors.WithStack(err)
	}
	if _, err := io.Copy(petsFile, &ignore); err != nil {
		petsFile.Close()
		return errors.WithStack(err)
	}
	if err := petsFile.Close(); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (a *App) CmdDiff(ctx context.Context, s System, stdout io.Writer) error {
	return nil
}

func getDefaultSigners() ([]ssh.Signer, error) {
	defaults := []string{
		filepath.Join(currentUser.HomeDir, ".ssh", "id_ed25519"),
		filepath.Join(currentUser.HomeDir, ".ssh", "id_rsa"),
		filepath.Join(currentUser.HomeDir, ".ssh", "id_ecdsa"),
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

func connectToHost(host string) (*ssh.Client, error) {
	config := &ssh.ClientConfig{
		// FIXME: make user optional for safer testing
		// User:            currentUser.Username,
		User:            "root",
		Auth:            getSSHAuthMethods(),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}

	addr := net.JoinHostPort(host, "22")
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

	if len(a.Names) == 0 {
		var err error
		a.Names, err = a.Discover()
		if err != nil {
			return err
		}
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
