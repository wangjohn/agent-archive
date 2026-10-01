package local

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// mountTablePath is where Linux lists this process's mounts.
const mountTablePath = "/proc/self/mountinfo"

// ReadMountTable reads this process's mount table (/proc/self/mountinfo, the
// Linux kernel's own account of what is mounted where, as this process sees
// it). It is for ParseMountTable.
func ReadMountTable() ([]byte, error) { return os.ReadFile(mountTablePath) }

// Mount is one line of the mount table: where a filesystem is mounted, and
// its type as the kernel names it ("ext4", "nfs4", "fuse.sshfs").
type Mount struct {
	Point string
	Type  string
}

// ParseMountTable reads the mount table in the format of proc(5)'s
// /proc/self/mountinfo, in the order it lists the mounts. A line is
//
//	36 35 98:0 /root /mnt/point rw,noatime master:1 - ext3 /dev/root rw
//
// that is: the mount ID, parent ID, device, the root of the mount inside its
// filesystem (what a bind mount of a directory makes differ from "/"), the
// mount point, the mount options, then optional fields up to a lone "-", and
// after it the filesystem type, the source and the super-block options. The
// kernel writes a space, a tab, a newline and a backslash in a path as a
// backslash and three octal digits ("\040"); they are decoded here. The
// fields are split at spaces only, which is all the kernel puts between them:
// a path may hold other bytes that are white space to Unicode (a no-break
// space, say), and the source may be empty. A line that is not in this format
// is skipped.
func ParseMountTable(data []byte) []Mount {
	var mounts []Mount
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.FieldsFunc(line, func(r rune) bool { return r == ' ' })
		// The six fields before the optional ones, the separator and the type.
		if len(fields) < 8 {
			continue
		}
		// The optional fields come first and end at the separator, so the
		// type is the field after the first "-" past the mount options.
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(fields) {
			continue
		}
		mounts = append(mounts, Mount{Point: unescapeMountPath(fields[4]), Type: fields[sep+1]})
	}
	return mounts
}

// unescapeMountPath undoes the kernel's octal escapes in a mount point. A
// backslash that does not start three octal digits is kept as it is.
func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// MountOf is the mount that holds path, which is cleaned and must be
// absolute: the one whose mount point is the longest prefix of it that ends
// at a path boundary ("/mnt/data" holds "/mnt/data/x" but not "/mnt/data2").
// Where two mounts share a mount point, the one listed last is the one on
// top, and holds the path. ok is false for a relative path or when no mount
// holds it.
func MountOf(mounts []Mount, path string) (mount Mount, ok bool) {
	if !filepath.IsAbs(path) {
		return Mount{}, false
	}
	path = filepath.Clean(path)
	best, bestLen := -1, -1
	for i, m := range mounts {
		point := filepath.Clean(m.Point)
		if !filepath.IsAbs(point) || !PathWithin(path, point) {
			continue
		}
		if len(point) >= bestLen {
			best, bestLen = i, len(point)
		}
	}
	if best < 0 {
		return Mount{}, false
	}
	return mounts[best], true
}

// networkFilesystems are the filesystem types (as the mount table names them)
// that more than one machine can have mounted at once, which is what makes a
// data directory on one unfit for this program: two machines would then share
// one machine ID and so claim the same sessions, their file locks (flock) are
// not reliable between machines, and each would run the background job
// against the same files.
//
// Only a type that is shared between machines by its nature is here. A type
// that merely may be, or that is a filesystem of this machine seen from
// elsewhere, is not:
//
//   - virtiofs is a host's folder shared into one virtual machine, and 9p is
//     the same (QEMU, Docker Desktop) and what WSL mounts Windows drives
//     with; neither says another machine mounts the same files, and a WSL
//     user's home is on the distribution's own disk. They are not blocked.
//   - a plain "fuse" (and fuse.gocryptfs, fuse.encfs, ecryptfs, fuse.mergerfs
//     and the like) is a program's view of local files, so it says nothing of
//     the machine; and fuseblk is a local block device (ntfs-3g).
//   - nfsd and rpc_pipefs are the kernel's own NFS server and client
//     plumbing, not a mounted share.
//   - autofs is an automount point that is not mounted yet; FilesystemProbe
//     mounts it before it reads the mount table, which then shows what is
//     mounted there.
//
// The network FUSE filesystems are listed by the type they mount as
// (fuse.<subtype>), which is what the mount table shows for them; MooseFS's
// mfsmount is fuse.mfs. OrangeFS's kernel client is pvfs2. gfs2 and ocfs2 are
// cluster filesystems on storage several machines attach to: the same shared
// home by another route.
var networkFilesystems = map[string]bool{
	"nfs": true, "nfs4": true,
	"cifs": true, "smb3": true, "smbfs": true,
	"ceph": true, "fuse.ceph-fuse": true,
	"glusterfs": true, "fuse.glusterfs": true,
	"afs": true, "lustre": true, "gpfs": true, "beegfs": true, "pvfs2": true,
	"gfs2": true, "ocfs2": true,
	"fuse.sshfs": true, "fuse.rclone": true, "fuse.s3fs": true, "fuse.gcsfuse": true,
	"fuse.mfs": true, "fuse.juicefs": true,
}

// NetworkFilesystem reports whether fstype, a filesystem type as the mount
// table names it, is a network (shared between machines) filesystem.
func NetworkFilesystem(fstype string) bool { return networkFilesystems[strings.ToLower(fstype)] }

// Placement is where a path is: the mount that holds it.
type Placement struct {
	// Path is the path that was asked about.
	Path string
	// Probed is Path with the symbolic links of the part that exists
	// resolved, which is what was looked up in the mount table (a directory
	// that setup has not made yet is on the filesystem of the nearest
	// directory above it that exists).
	Probed string
	// Mount is the mount point of the filesystem that holds Probed, and Type
	// that filesystem's type.
	Mount string
	Type  string
}

// Network reports whether the filesystem is a network one.
func (p Placement) Network() bool { return NetworkFilesystem(p.Type) }

// FilesystemProbe finds what filesystem a path is on. Its zero value reads
// the real mount table and file system; a test replaces either.
type FilesystemProbe struct {
	// MountTable returns the text of the mount table. Nil reads
	// /proc/self/mountinfo.
	MountTable func() ([]byte, error)
	// EvalSymlinks resolves path's symbolic links. Nil is
	// filepath.EvalSymlinks, then a look inside the directory it names, which
	// mounts it if it is an automount point (autofs) not mounted yet.
	EvalSymlinks func(path string) (string, error)
}

// resolveAndMount is filepath.EvalSymlinks, and then a stat of what it named
// through a trailing slash. Resolving a path mounts every automount point
// (autofs, the usual way NFS homes are mounted) that the path passes through,
// but not the last one, which a stat leaves as it is; a trailing slash makes
// the kernel look inside the directory, which mounts it as making a directory
// in it would. Its error is ignored: the resolved path is the answer either
// way.
func resolveAndMount(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		_, _ = os.Stat(strings.TrimSuffix(resolved, string(filepath.Separator)) + string(filepath.Separator))
	}
	return resolved, err
}

// Where reports what filesystem path is on, or ok=false when that cannot be
// told: the mount table cannot be read (a system without /proc, or one whose
// /proc is hidden), path is not absolute, or the part of it that exists
// cannot be looked at. An answer that cannot be told is never a reason to
// refuse anything: callers treat it as "not a network filesystem".
//
// The path need not exist: the nearest ancestor that does is used, so the
// answer for a directory not made yet is its parent's, which is where it
// will be made. Symbolic links in that ancestor are resolved first, so a data
// directory that is a link to a mounted share is found on the share. A link
// whose target does not exist counts as not there, so the answer is its
// parent's: making a directory through it fails, so nothing is written
// where it points.
//
// The path is resolved before the mount table is read, since resolving it is
// what mounts an automount point on the way (an autofs home not mounted
// yet): read first, the table would show the automount point's own type,
// autofs, which is not a network filesystem.
func (f FilesystemProbe) Where(path string) (Placement, bool) {
	if !filepath.IsAbs(path) {
		return Placement{}, false
	}
	read, resolve := f.MountTable, f.EvalSymlinks
	if read == nil {
		read = ReadMountTable
	}
	if resolve == nil {
		resolve = resolveAndMount
	}
	probed, ok := nearestExisting(resolve, filepath.Clean(path))
	if !ok {
		return Placement{}, false
	}
	data, err := read()
	if err != nil {
		return Placement{}, false
	}
	mount, ok := MountOf(ParseMountTable(data), probed)
	if !ok {
		return Placement{}, false
	}
	return Placement{Path: path, Probed: probed, Mount: mount.Point, Type: mount.Type}, true
}

// nearestExisting resolves the nearest ancestor of path (path itself first)
// that exists, and returns it with the names below it that do not, joined
// back on. Anything but "not there" (a permission error on a directory, say)
// is an answer that cannot be told, not a reason to look higher: the
// directory above might be on another filesystem than the one that is
// unreadable.
func nearestExisting(resolve func(string) (string, error), path string) (string, bool) {
	var missing []string
	for current := path; ; {
		resolved, err := resolve(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, true
		}
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return "", false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}
