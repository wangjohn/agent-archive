package local

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Every type the mount table can show is either a network filesystem or not,
// and each is pinned here by itself, so moving one across the line is a
// decision this test shows. The reasons are in networkFilesystems.
func TestNetworkFilesystemClassification(t *testing.T) {
	t.Parallel()
	for _, fstype := range []string{
		"nfs", "nfs4", "cifs", "smb3", "smbfs", "ceph", "fuse.ceph-fuse",
		"glusterfs", "fuse.glusterfs", "afs", "lustre", "gpfs", "beegfs",
		"gfs2", "ocfs2", "fuse.sshfs", "fuse.rclone", "fuse.s3fs", "fuse.gcsfuse",
		"pvfs2", "fuse.mfs", "fuse.juicefs",
	} {
		if !NetworkFilesystem(fstype) {
			t.Errorf("%s is shared between machines and is not classed as a network filesystem", fstype)
		}
	}
	for _, fstype := range []string{
		// Local disks and memory.
		"ext4", "ext3", "xfs", "btrfs", "zfs", "f2fs", "tmpfs", "overlay", "squashfs", "vfat", "exfat", "ntfs3", "fuseblk",
		// Host-to-guest sharing, which names no other machine.
		"virtiofs", "9p", "vboxsf", "vmhgfs", "fuse.vmhgfs-fuse",
		// A program's view of local files.
		"fuse", "fuse.gocryptfs", "fuse.encfs", "ecryptfs", "fuse.mergerfs", "fuse.portal",
		// The kernel's NFS plumbing, not a share.
		"nfsd", "rpc_pipefs",
		// An automount point not mounted yet (the probe mounts it first).
		"autofs",
		// Nothing known.
		"", "unknown",
	} {
		if NetworkFilesystem(fstype) {
			t.Errorf("%q is classed as a network filesystem, but a home on it is not shared between machines by its nature", fstype)
		}
	}
	if !NetworkFilesystem("NFS4") {
		t.Error("the type's case changes the answer")
	}
}

const mountTable = `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro
23 22 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:5 - proc proc rw
40 22 0:40 / /home rw,relatime shared:20 - nfs4 server:/export/home rw,vers=4.2,sec=sys
41 40 0:41 / /home/me/local rw,relatime - ext4 /dev/sdb1 rw
42 22 0:42 /sub /srv/bound rw,relatime shared:22 - nfs4 server:/export/sub rw
43 22 0:43 / /mnt/my\040share rw,relatime - cifs //nas/share rw,vers=3.1.1
44 22 0:44 / /mnt/tab\011and\134slash rw - fuse.sshfs me@host:/ rw,user_id=1000
45 22 0:45 / /mnt/data rw,relatime - xfs /dev/sdc1 rw
46 22 0:46 / /mnt/data2 rw,relatime - nfs server:/data2 rw
47 40 0:47 / /home/me/overmounted rw shared:1 master:2 propagate_from:3 - ext4 /dev/sdd1 rw
48 40 0:48 / /home/me/overmounted rw - nfs4 server:/again rw
`

func TestParseMountTable(t *testing.T) {
	t.Parallel()
	got := ParseMountTable([]byte(mountTable))
	want := []Mount{
		{"/", "ext4"}, {"/proc", "proc"}, {"/home", "nfs4"}, {"/home/me/local", "ext4"},
		{"/srv/bound", "nfs4"}, {"/mnt/my share", "cifs"}, {"/mnt/tab\tand\\slash", "fuse.sshfs"},
		{"/mnt/data", "xfs"}, {"/mnt/data2", "nfs"}, {"/home/me/overmounted", "ext4"}, {"/home/me/overmounted", "nfs4"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d mounts %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mount %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseMountTableSkipsWhatItCannotRead(t *testing.T) {
	t.Parallel()
	for name, table := range map[string]string{
		"empty":            "",
		"blank lines":      "\n\n",
		"too short":        "35 26 0:30 / /mnt rw\n",
		"no separator":     "35 26 0:30 / /mnt rw shared:1 tmpfs tmpfs rw extra\n",
		"nothing after it": "35 26 0:30 / /mnt rw shared:1 a b c d -\n",
		"binary noise":     "\x00\xff\xfe\n",
	} {
		if mounts := ParseMountTable([]byte(table)); len(mounts) != 0 {
			t.Errorf("%s: parsed %v", name, mounts)
		}
	}
	// Fields are split at spaces only: a no-break space (white space to
	// Unicode, a byte sequence like any other to the kernel) stays in the
	// path, and an empty source (two spaces) does not lose the line.
	got := ParseMountTable([]byte("35 26 0:30 / /mnt/no\u00a0break rw - nfs4 s:/e rw\n" +
		"36 26 0:31 / /mnt/no-source rw - nfs  rw\n"))
	if want := []Mount{{"/mnt/no\u00a0break", "nfs4"}, {"/mnt/no-source", "nfs"}}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("parsed %q, want %q", got, want)
	}
	// A backslash that does not start three octal digits stays a backslash.
	got = ParseMountTable([]byte(`35 26 0:30 / /mnt/a\9b\04 rw - ext4 /dev/x rw` + "\n"))
	if len(got) != 1 || got[0].Point != `/mnt/a\9b\04` {
		t.Errorf("a malformed escape gives %v", got)
	}
}

// The mount that holds a path is the one with the longest mount point that
// is the path or a directory above it: a mount below it (a local disk in an
// NFS home) wins over the one around it, a bind mount's own mount point is
// the one that counts, "/mnt/data" does not hold "/mnt/data2", and of two
// mounts on one mount point the later is the one on top.
func TestMountOfFindsTheLongestMountPoint(t *testing.T) {
	t.Parallel()
	mounts := ParseMountTable([]byte(mountTable))
	for _, tc := range []struct {
		name   string
		path   string
		point  string
		fstype string
	}{
		{"the root", "/etc/hosts", "/", "ext4"},
		{"under a mount", "/home/me/.local/share/agent-archive", "/home", "nfs4"},
		{"the mount point itself", "/home", "/home", "nfs4"},
		{"a local disk mounted inside a network one", "/home/me/local/agent-archive", "/home/me/local", "ext4"},
		{"a bind mount of a part of a share", "/srv/bound/x/y", "/srv/bound", "nfs4"},
		{"a mount point with a space", "/mnt/my share/dir", "/mnt/my share", "cifs"},
		{"a mount point with a tab and a backslash", "/mnt/tab\tand\\slash/d", "/mnt/tab\tand\\slash", "fuse.sshfs"},
		{"a name that only begins like a mount point", "/mnt/data2/x", "/mnt/data2", "nfs"},
		{"the shorter name is not a prefix of the longer", "/mnt/data/x", "/mnt/data", "xfs"},
		{"a name that begins like one but is neither", "/mnt/data3/x", "/", "ext4"},
		{"two mounts on one mount point", "/home/me/overmounted/x", "/home/me/overmounted", "nfs4"},
		{"a path that is not clean", "/home/me/local/../x//y/.", "/home", "nfs4"},
	} {
		got, ok := MountOf(mounts, tc.path)
		if !ok || got.Point != tc.point || got.Type != tc.fstype {
			t.Errorf("%s: MountOf(%q) = %+v, %v; want %s on %s", tc.name, tc.path, got, ok, tc.fstype, tc.point)
		}
	}
	if _, ok := MountOf(mounts, "relative/path"); ok {
		t.Error("a relative path was placed")
	}
	if _, ok := MountOf(nil, "/home"); ok {
		t.Error("a path was placed with no mounts")
	}
}

// The mount table is the process's, so a path is looked up as the nearest
// directory that exists, with its links resolved.
func TestFilesystemProbeUsesTheNearestExistingAncestor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(resolvedRoot, "home")
	nfs := filepath.Join(resolvedRoot, "nfs")
	for _, dir := range []string{home, nfs, filepath.Join(nfs, "deeper")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// ~/link is a link to a directory on the share.
	if err := os.Symlink(filepath.Join(nfs, "deeper"), filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	table := []byte("1 0 8:1 / / rw - ext4 /dev/sda rw\n2 1 0:9 / " + nfs + " rw - nfs4 s:/e rw\n")
	probe := FilesystemProbe{MountTable: func() ([]byte, error) { return table, nil }}

	for _, tc := range []struct {
		name   string
		path   string
		fstype string
		probed string
	}{
		{"a directory that exists", home, "ext4", home},
		{"one that does not, several levels down", filepath.Join(home, "a", "b", "c"), "ext4", filepath.Join(home, "a", "b", "c")},
		{"one that does not, on the share", filepath.Join(nfs, "new", "dir"), "nfs4", filepath.Join(nfs, "new", "dir")},
		{"a link to the share", filepath.Join(home, "link"), "nfs4", filepath.Join(nfs, "deeper")},
		{"a directory below a link to the share, not made yet", filepath.Join(home, "link", "new"), "nfs4", filepath.Join(nfs, "deeper", "new")},
		{"a path with a file in the way", filepath.Join(home, "file", "below"), "ext4", filepath.Join(home, "file", "below")},
	} {
		path := tc.path
		got, ok := probe.Where(path)
		if !ok || got.Type != tc.fstype || got.Probed != tc.probed || got.Path != path {
			t.Errorf("%s: Where(%q) = %+v, %v; want %s probed as %s", tc.name, path, got, ok, tc.fstype, tc.probed)
		}
	}
	if got, _ := probe.Where(filepath.Join(nfs, "x")); !got.Network() || got.Mount != nfs {
		t.Errorf("a path on the share is %+v", got)
	}
	if got, _ := probe.Where(home); got.Network() {
		t.Errorf("a path off the share is %+v", got)
	}
}

// What cannot be told is reported as such, never as an answer.
func TestFilesystemProbeSaysWhenItCannotTell(t *testing.T) {
	t.Parallel()
	table := func() ([]byte, error) { return []byte("1 0 8:1 / / rw - nfs4 s:/e rw\n"), nil }
	resolveTo := func(err error) func(string) (string, error) {
		return func(string) (string, error) { return "", err }
	}
	for name, probe := range map[string]FilesystemProbe{
		"no mount table":           {MountTable: func() ([]byte, error) { return nil, fs.ErrNotExist }},
		"an empty mount table":     {MountTable: func() ([]byte, error) { return nil, nil }},
		"a directory not readable": {MountTable: table, EvalSymlinks: resolveTo(fs.ErrPermission)},
		"nothing exists":           {MountTable: table, EvalSymlinks: resolveTo(&fs.PathError{Op: "lstat", Path: "/", Err: syscall.ENOENT})},
	} {
		if got, ok := probe.Where("/home/me/.local/share/agent-archive"); ok {
			t.Errorf("%s: placed at %+v", name, got)
		}
	}
	if _, ok := (FilesystemProbe{MountTable: table}).Where("relative"); ok {
		t.Error("a relative path was placed")
	}
	// With the real file system and a table that holds nothing the path is
	// on, there is no answer either.
	probe := FilesystemProbe{MountTable: func() ([]byte, error) {
		return []byte("1 0 8:1 / /elsewhere rw - nfs4 s:/e rw\n"), nil
	}}
	if got, ok := probe.Where(t.TempDir()); ok {
		t.Errorf("a path under no mount was placed at %+v", got)
	}
}

// ENOTDIR (a file where a directory is expected) and ENOENT both mean "not
// there", so the path is looked up at what does exist above it.
func TestNearestExistingStopsAtThePartThatExists(t *testing.T) {
	t.Parallel()
	exists := map[string]string{"/a": "/real/a"}
	resolve := func(p string) (string, error) {
		if r, ok := exists[p]; ok {
			return r, nil
		}
		if strings.HasPrefix(p, "/a/file/") {
			return "", syscall.ENOTDIR
		}
		return "", errors.Join(fs.ErrNotExist)
	}
	got, ok := nearestExisting(resolve, "/a/file/x/y")
	if !ok || got != "/real/a/file/x/y" {
		t.Errorf("nearestExisting = %q, %v", got, ok)
	}
	if _, ok := nearestExisting(resolve, "/b/c"); ok {
		t.Error("a path with nothing that exists was resolved")
	}
}

// A link whose target is missing is not there: the answer is the link's
// parent's (making a directory through such a link fails, so nothing lands
// where it points), and a relative link is resolved from its own directory.
func TestFilesystemProbeFollowsRelativeLinksAndNotDanglingOnes(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, nfs := filepath.Join(root, "home"), filepath.Join(root, "nfs")
	for _, dir := range []string{home, nfs} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("..", "nfs"), filepath.Join(home, "relative")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(nfs, "gone"), filepath.Join(home, "dangling")); err != nil {
		t.Fatal(err)
	}
	table := []byte("1 0 8:1 / / rw - ext4 /dev/sda rw\n2 1 0:9 / " + nfs + " rw - nfs4 s:/e rw\n")
	probe := FilesystemProbe{MountTable: func() ([]byte, error) { return table, nil }}
	if got, ok := probe.Where(filepath.Join(home, "relative", "agent-archive")); !ok || !got.Network() || got.Probed != filepath.Join(nfs, "agent-archive") {
		t.Errorf("a relative link to the share: %+v, %v", got, ok)
	}
	if got, ok := probe.Where(filepath.Join(home, "dangling", "agent-archive")); !ok || got.Network() || got.Probed != filepath.Join(home, "dangling", "agent-archive") {
		t.Errorf("a dangling link: %+v, %v", got, ok)
	}
	if err := os.MkdirAll(filepath.Join(home, "dangling", "agent-archive"), 0o700); err == nil {
		t.Error("a directory was made through a dangling link")
	}
	if _, err := os.Stat(filepath.Join(nfs, "gone")); !os.IsNotExist(err) {
		t.Errorf("making a directory through a dangling link reached its target (%v)", err)
	}
}

// Resolving the path is what mounts an automount point on the way, so the
// mount table is read after it: read before, an autofs home not mounted yet
// would show as autofs, not as the NFS mounted there a moment later.
func TestFilesystemProbeReadsTheMountTableAfterResolving(t *testing.T) {
	t.Parallel()
	mounted := false
	probe := FilesystemProbe{
		MountTable: func() ([]byte, error) {
			table := "1 0 8:1 / / rw - ext4 /dev/sda rw\n2 1 0:30 / /home rw - autofs auto.home rw\n"
			if mounted {
				table += "3 2 0:31 / /home/me rw - nfs4 server:/export/me rw\n"
			}
			return []byte(table), nil
		},
		EvalSymlinks: func(path string) (string, error) {
			mounted = true
			return path, nil
		},
	}
	if got, ok := probe.Where("/home/me/.local/share/agent-archive"); !ok || !got.Network() || got.Mount != "/home/me" {
		t.Errorf("an automounted home: %+v, %v", got, ok)
	}
}

// The default resolver looks inside the directory the walk ends on, through a
// trailing slash (what mounts an autofs point that a plain stat leaves
// alone), and only at a path that exists: a file's "not a directory" is
// ignored, and a missing path or a dangling link is never looked inside. It
// replaces statInside, so it is not parallel.
func TestFilesystemProbeLooksInsideThePathItEndsOn(t *testing.T) {
	var looked []string
	t.Cleanup(func() { statInside = os.Stat })
	statInside = func(path string) (fs.FileInfo, error) {
		looked = append(looked, path)
		return os.Stat(path)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "gone"), filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	table := []byte("1 0 8:1 / / rw - ext4 /dev/sda rw\n")
	probe := FilesystemProbe{MountTable: func() ([]byte, error) { return table, nil }}
	for _, tc := range []struct {
		path   string
		probed string
		looked string
	}{
		{root, root, root + "/"},
		{filepath.Join(root, "missing", "agent-archive"), filepath.Join(root, "missing", "agent-archive"), root + "/"},
		{file, file, file + "/"},
		{filepath.Join(root, "dangling"), filepath.Join(root, "dangling"), root + "/"},
		{"/", "/", "/"},
	} {
		looked = nil
		got, ok := probe.Where(tc.path)
		if !ok || got.Probed != tc.probed || got.Type != "ext4" {
			t.Errorf("%s: %+v, %v", tc.path, got, ok)
		}
		if len(looked) != 1 || looked[0] != tc.looked {
			t.Errorf("%s: looked inside %q, want only %q", tc.path, looked, tc.looked)
		}
	}
}
