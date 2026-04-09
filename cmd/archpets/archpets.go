package main

import (
	"bufio"
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/daaku/prefixer"
	"github.com/jpillora/opts"
	"github.com/pelletier/go-toml/v2"
	"github.com/pkg/diff"
	"github.com/pkg/diff/write"
	"github.com/pkg/errors"
	"github.com/pkg/sftp/v2"
	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

var currentUser, _ = user.Current()

type didDiffWriter struct {
	io.Writer
	header   []byte
	newlines int
}

var newline = []byte("\n")

func (d *didDiffWriter) Write(b []byte) (int, error) {
	d.newlines += bytes.Count(b, newline)
	if !d.isDiff() {
		d.header = append(d.header, b...)
		return len(b), nil
	}
	if len(d.header) > 0 {
		if n, err := d.Write(d.header); err != nil {
			return n, err
		}
		d.header = nil
	}
	return d.Writer.Write(b)
}

func (d *didDiffWriter) isDiff() bool {
	return d.newlines > 2
}

type FileMeta struct {
	User  string      `toml:"user"`
	Group string      `toml:"group"`
	Mode  fs.FileMode `toml:"mode"`
}

func (m *FileMeta) ownerString() string {
	return fmt.Sprintf("%d:%d", m.User, m.Group)
}

func (m *FileMeta) allString() string {
	return fmt.Sprintf("%d:%d %s", m.User, m.Group, m.Mode)
}

func FileMetaFromStat(stat fs.FileInfo, ug *userGroupMap) (FileMeta, error) {
	attr, ok := stat.Sys().(*sshfx.Attributes)
	if !ok {
		return FileMeta{}, errors.Errorf("expected %T, but got %T for %s", attr, stat.Sys(), stat.Name())
	}
	user, err := ug.user(int(attr.UID))
	if err != nil {
		return FileMeta{}, err
	}
	group, err := ug.group(int(attr.GID))
	if err != nil {
		return FileMeta{}, err
	}
	return FileMeta{
		User:  user,
		Group: group,
		Mode:  stat.Mode().Perm(),
	}, nil
}

type SourceConfig struct {
	Files map[string]FileMeta `toml:"files"`
}

func (c *SourceConfig) FileMeta(relDestPath, sourcePath string, dir bool) (FileMeta, error) {
	mode := fs.FileMode(0o644)
	if dir {
		mode = 0o755
	} else {
		// maintain executable bit if set in filesystem
		stat, err := os.Stat(sourcePath)
		if err != nil {
			return FileMeta{}, errors.WithStack(err)
		}
		if stat.Mode()&0o111 != 0 {
			mode = 0o755
		}
	}
	for pattern, meta := range c.Files {
		if meta.Group == "" {
			meta.Group = "root"
		}
		if meta.User == "" {
			meta.User = "root"
		}
		match, err := doublestar.Match(pattern, relDestPath)
		if err != nil {
			return FileMeta{}, errors.WithStack(err)
		}
		if match {
			if meta.Mode == 0 {
				meta.Mode = mode
			} else if dir {
				// for any read bits set, also set the execute bit
				for _, readBit := range []fs.FileMode{0o400, 0o040, 0o004} {
					if meta.Mode&readBit != 0 {
						meta.Mode |= readBit >> 2 // shift read bit to execute bit for that group
					}
				}
			}
			return meta, nil
		}
	}
	// defaults
	return FileMeta{
		User:  "root",
		Group: "root",
		Mode:  mode,
	}, nil
}

type File interface {
	String() string
	DestPath() string
	SourcePath() string
	DesiredMeta() FileMeta
	Run(client *sftp.Client, ug *userGroupMap) error
}

func Diff(client *sftp.Client, ug *userGroupMap, f File, out io.Writer, options ...write.Option) (bool, error) {
	var destMeta FileMeta
	destStat, err := client.LStat(f.DestPath())
	if err != nil {
		if !serrors.Is(err, fs.ErrNotExist) {
			return false, errors.WithStack(err)
		}
	} else {
		destMeta, err = FileMetaFromStat(destStat, ug)
		if err != nil {
			return false, errors.WithStack(err)
		}
	}
	desiredMeta := f.DesiredMeta()

	var destMetaBuf, desiredMetaBuf bytes.Buffer
	switch f := f.(type) {
	default:
		return false, errors.Errorf("unknown File type: %T", f)
	case *FileCopy:
		fmt.Fprintln(&destMetaBuf, destMeta.allString())
		fmt.Fprintln(&desiredMetaBuf, desiredMeta.allString())

		destReader, err := client.Open(f.DestPath())
		if err != nil {
			if !serrors.Is(err, fs.ErrNotExist) {
				return false, errors.WithStack(err)
			}
		} else {
			if _, err := io.Copy(&destMetaBuf, destReader); err != nil {
				return false, errors.WithStack(err)
			}
		}

		desiredReader, err := os.Open(f.sourcePath)
		if err != nil {
			return false, errors.WithStack(err)
		}
		if _, err := io.Copy(&desiredMetaBuf, desiredReader); err != nil {
			return false, errors.WithStack(err)
		}
	case *FileMkdir:
		fmt.Fprintln(&destMetaBuf, destMeta.allString())
		fmt.Fprintln(&desiredMetaBuf, desiredMeta.allString())
	case *FileSymlink:
		fmt.Fprintln(&destMetaBuf, destMeta.ownerString())
		fmt.Fprintln(&desiredMetaBuf, desiredMeta.ownerString())

		if destStat != nil && !destStat.IsDir() && !destStat.Mode().IsRegular() {
			currentLink, err := client.ReadLink(f.destPath)
			if err != nil {
				if !serrors.Is(err, fs.ErrNotExist) {
					return false, errors.WithStack(err)
				}
			}
			fmt.Fprintln(&destMetaBuf, currentLink)
		}

		fmt.Fprintln(&desiredMetaBuf, f.targetPath)
	}

	writer := didDiffWriter{Writer: out}
	err = diff.Text(f.DestPath(), f.SourcePath(), destMetaBuf.Bytes(), desiredMetaBuf.Bytes(), &writer, options...)
	if err != nil {
		return false, errors.WithStack(err)
	}
	return writer.isDiff(), nil
}

func atomicallyReplace(
	client *sftp.Client,
	ug *userGroupMap,
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

	uid, err := ug.uid(meta.User)
	if err != nil {
		return err
	}
	gid, err := ug.gid(meta.Group)
	if err != nil {
		return err
	}
	if err := client.Chown(tempPath, uid, gid); err != nil {
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
	sourcePath string
	destPath   string
	meta       FileMeta
}

func (t *FileMkdir) String() string {
	return fmt.Sprintf("M %s", t.destPath)
}

func (t *FileMkdir) DestPath() string {
	return t.destPath
}

func (t *FileMkdir) SourcePath() string {
	return t.sourcePath
}

func (t *FileMkdir) DesiredMeta() FileMeta {
	return t.meta
}

func (t *FileMkdir) Run(client *sftp.Client, ug *userGroupMap) error {
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
		if stat.Mode().Perm() != t.meta.Mode.Perm() {
			if err := client.Chmod(t.destPath, t.meta.Mode.Perm()); err != nil {
				return errors.WithStack(err)
			}
		}
	}
	uid, err := ug.uid(t.meta.User)
	if err != nil {
		return err
	}
	gid, err := ug.gid(t.meta.Group)
	if err != nil {
		return err
	}
	if err := client.Chown(t.destPath, uid, gid); err != nil {
		return errors.WithStack(err)
	}
	return nil
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

func (t *FileCopy) SourcePath() string {
	return t.sourcePath
}

func (t *FileCopy) DesiredMeta() FileMeta {
	return t.meta
}

func (t *FileCopy) Run(client *sftp.Client, ug *userGroupMap) error {
	src, err := os.Open(t.sourcePath)
	if err != nil {
		return errors.WithStack(err)
	}
	defer src.Close()
	return atomicallyReplace(client, ug, t.destPath, t.meta, src)
}

type FileSymlink struct {
	sourcePath string
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

func (t *FileSymlink) SourcePath() string {
	return t.sourcePath
}

func (t *FileSymlink) DesiredMeta() FileMeta {
	return t.meta
}

func (t *FileSymlink) Run(client *sftp.Client, ug *userGroupMap) error {
	client.Remove(t.destPath) // ignore error
	if err := client.Symlink(t.targetPath, t.destPath); err != nil {
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

func (s *System) FileMeta(destPath, sourcePath string, dir bool) (FileMeta, error) {
	relPath, err := filepath.Rel(s.Root+"/", destPath)
	if err != nil {
		return FileMeta{}, errors.WithStack(err)
	}
	return s.SourceConfig.FileMeta(relPath, sourcePath, dir)
}

func (s *System) walkFunc(files *mutSlice[File], path, destPath string, d fs.DirEntry, err error) error {
	if err != nil {
		return errors.WithStack(err)
	}

	meta, err := s.FileMeta(destPath, path, d.IsDir())
	if err != nil {
		return err
	}

	if d.IsDir() {
		if destPath != "/" {
			files.append(&FileMkdir{
				sourcePath: path,
				destPath:   destPath,
				meta:       meta,
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
				sourcePath: path,
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
				sourcePath: path,
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
	Color   bool     `opts:"name=color,help=colorized output"`
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

type userGroupMap struct {
	userToID   map[string]int
	groupToGID map[string]int
	idToUser   map[int]string
	gidToGroup map[int]string
}

func (ug *userGroupMap) user(id int) (string, error) {
	user, found := ug.idToUser[id]
	if !found {
		return "", errors.Errorf("couldnt map user %d to name", id)
	}
	return user, nil
}

func (ug *userGroupMap) group(id int) (string, error) {
	group, found := ug.gidToGroup[id]
	if !found {
		return "", errors.Errorf("couldnt map group %d to name", id)
	}
	return group, nil
}

func (ug *userGroupMap) uid(user string) (int, error) {
	uid, found := ug.userToID[user]
	if !found {
		return 0, errors.Errorf("couldnt map user %s to id", user)
	}
	return uid, nil
}

func (ug *userGroupMap) gid(group string) (int, error) {
	gid, found := ug.groupToGID[group]
	if !found {
		return 0, errors.Errorf("couldnt map group %s to gid", group)
	}
	return gid, nil
}

func buildUserGroupMap(c *sftp.Client) (*userGroupMap, error) {
	var ug userGroupMap
	for _, t := range []struct {
		strToInt *map[string]int
		intToStr *map[int]string
		filename string
	}{
		{&ug.userToID, &ug.idToUser, "/etc/passwd"},
		{&ug.groupToGID, &ug.gidToGroup, "/etc/group"},
	} {
		f, err := c.Open(t.filename)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		scanner := bufio.NewScanner(f)
		l := 0
		strToInt := make(map[string]int)
		intToStr := make(map[int]string)
		for scanner.Scan() {
			parts := strings.SplitN(scanner.Text(), ":", 4)
			if len(parts) != 4 {
				return nil, errors.Errorf("error parsing line number %d in file %q", l, t.filename)
			}
			v, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil {
				return nil, errors.Errorf("error parsing id in line number %d in file %q", l, t.filename)
			}
			strToInt[parts[0]] = int(v)
			intToStr[int(v)] = parts[0]
			l++
		}
		if err := scanner.Err(); err != nil {
			return nil, errors.Errorf("error parsing line number %d in file %q: %v", l, t.filename, err)
		}
		*t.intToStr = intToStr
		*t.strToInt = strToInt
	}
	return &ug, nil
}

func (a *App) diffOrDeploy(ctx context.Context, s System, stdout io.Writer, diffMode bool) error {
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
	ug, err := buildUserGroupMap(sftpClient)
	if err != nil {
		return err
	}
	files, err := s.Files()
	if err != nil {
		return err
	}
	diffOut := io.Discard
	if diffMode {
		diffOut = stdout
	}
	var diffOptions []write.Option
	if a.Color {
		diffOptions = append(diffOptions, write.TerminalColor())
	}
	var ignore bytes.Buffer
	for _, f := range files {
		ignorePath := f.DestPath()
		if a.Root != "/" {
			ignorePath = strings.TrimPrefix(ignorePath, a.Root)
		}

		// dont ignore directories
		if _, ok := f.(*FileMkdir); !ok {
			fmt.Fprintln(&ignore, ignorePath)
		}

		diff, err := Diff(sftpClient, ug, f, diffOut, diffOptions...)
		if err != nil {
			return err
		}
		if !diff {
			if a.Verbose {
				fmt.Fprintf(stdout, "unchanged: %s\n", f.String())
			}
			continue
		}
		if !diffMode || a.Verbose {
			fmt.Fprintf(stdout, "changed: %s\n", f.String())
		}
		if !diffMode {
			if err := f.Run(sftpClient, ug); err != nil {
				return err
			}
		}
	}

	if !diffMode {
		// FIXME only update if changed
		petsIgnoreFilename := filepath.Join(a.Root, "/etc/archdiff/pets")
		fmt.Fprintln(&ignore, petsIgnoreFilename)
		petsMeta := FileMeta{Mode: fs.FileMode(0o644)}
		if err := atomicallyReplace(sftpClient, ug, petsIgnoreFilename, petsMeta, &ignore); err != nil {
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
		Root:  "/",
		Repo:  "/home/naitik/workspace/pets",
		Color: true,
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
