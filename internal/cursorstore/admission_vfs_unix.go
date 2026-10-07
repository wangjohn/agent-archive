//go:build darwin || linux

package cursorstore

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Admission uses a private Unix VFS wrapper. SQLite's ordinary VFS stays
// untouched. Native descriptors are verified before reading; destination
// writes and truncation are checked before delegation, including final flush.
type admissionVFS struct {
	name              string
	ptr               uintptr
	namePtr           uintptr
	base              uintptr
	native            sqlite3.Tsqlite3_vfs
	clone             sqlite3.Tsqlite3_vfs
	pin               runtime.Pinner
	allowed           map[string]os.FileInfo
	limit             int64
	writable          bool
	heldFile          *os.File
	context           context.Context
	writeCalls        atomic.Int64
	writeBytes        atomic.Int64
	maxWriteEnd       atomic.Int64
	deniedWrites      atomic.Int64
	opens             atomic.Int64
	deniedOpens       atomic.Int64
	activeDescriptors atomic.Int64
}

type admissionFile struct {
	vfs             *admissionVFS
	original        uintptr
	originalMethods sqlite3.Tsqlite3_io_methods
	table           sqlite3.Tsqlite3_io_methods
	methods         uintptr
	pin             runtime.Pinner
}

var admissionVFSState = struct {
	sync.Mutex
	vfs   map[uintptr]*admissionVFS
	files map[uintptr]*admissionFile
}{vfs: map[uintptr]*admissionVFS{}, files: map[uintptr]*admissionFile{}}
var admissionVFSSequence atomic.Uint64

func callbackPointer[T any](fn T) uintptr { return *(*uintptr)(unsafe.Pointer(&struct{ f T }{fn})) }

func loadAdmissionC[T any](tls *libc.TLS, ptr uintptr) T {
	var value T
	target := unsafe.Slice((*byte)(unsafe.Pointer(&value)), int(unsafe.Sizeof(value)))
	copy(target, libc.GoBytes(ptr, len(target)))
	return value
}

func setAdmissionMethods(tls *libc.TLS, file, methods uintptr) {
	writeAdmissionC(tls, file, sqlite3.Tsqlite3_file{FpMethods: methods})
}

func newAdmissionVFS(allowed map[string]os.FileInfo, writable bool, limit int64) (*admissionVFS, error) {
	if !writable && os.Geteuid() == 0 {
		return nil, errors.New("read-only cursor admission cannot delegate Unix SHM ownership changes as root")
	}
	tls := libc.NewTLS()
	defer tls.Close()
	base := sqlite3.Xsqlite3_vfs_find(tls, 0)
	if base == 0 {
		return nil, errors.New("bounded admission VFS unavailable")
	}
	native := loadAdmissionC[sqlite3.Tsqlite3_vfs](tls, base)
	if libc.GoString(native.FzName) != "unix" || native.FszOsFile != int32(unsafe.Sizeof(sqlite3.TunixFile{})) || native.FxOpen == 0 {
		return nil, errors.New("bounded admission requires the known Unix VFS ABI")
	}
	name := "archive-admission-" + itoa(admissionVFSSequence.Add(1))
	namePtr, err := libc.CString(name)
	if err != nil {
		return nil, err
	}
	v := &admissionVFS{name: name, namePtr: namePtr, base: base, allowed: allowed, writable: writable, limit: limit, native: native, clone: native}
	v.clone.FzName = namePtr
	v.clone.FpNext = 0
	v.clone.FxOpen = callbackPointer(admissionOpen)
	v.clone.FxDelete = callbackPointer(admissionDelete)
	v.pin.Pin(&v.clone)
	v.ptr = libc.Xmalloc(tls, libc.Tsize_t(unsafe.Sizeof(v.clone)))
	if v.ptr == 0 {
		v.pin.Unpin()
		libc.Xfree(tls, namePtr)
		return nil, errors.New("bounded admission VFS allocation failed")
	}
	writeAdmissionC(tls, v.ptr, v.clone)
	admissionVFSState.Lock()
	admissionVFSState.vfs[v.ptr] = v
	admissionVFSState.Unlock()
	if sqlite3.Xsqlite3_vfs_register(tls, v.ptr, 0) != sqlite3.SQLITE_OK {
		v.Close()
		return nil, errors.New("bounded admission VFS registration failed")
	}
	return v, nil
}

func newAdmissionDestinationVFS(ctx context.Context, path string, held *os.File, limit int64) (*admissionVFS, error) {
	info, err := held.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("bounded destination descriptor unavailable")
	}
	v, err := newAdmissionVFS(map[string]os.FileInfo{path: info}, true, limit)
	if err != nil {
		return nil, err
	}
	v.heldFile = held
	v.context = ctx
	v.clone.FxFullPathname = callbackPointer(admissionFullPathname)
	tls := libc.NewTLS()
	writeAdmissionC(tls, v.ptr, v.clone)
	tls.Close()
	return v, nil
}

func itoa(n uint64) string {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			return string(b[i:])
		}
	}
}

func (v *admissionVFS) Close() error {
	if v.activeDescriptors.Load() != 0 {
		return errors.New("cursor admission native handles remain open")
	}
	tls := libc.NewTLS()
	defer tls.Close()
	if sqlite3.Xsqlite3_vfs_unregister(tls, v.ptr) != sqlite3.SQLITE_OK {
		return errors.New("cursor admission VFS cleanup failed")
	}
	admissionVFSState.Lock()
	delete(admissionVFSState.vfs, v.ptr)
	admissionVFSState.Unlock()
	v.pin.Unpin()
	libc.Xfree(tls, v.ptr)
	libc.Xfree(tls, v.namePtr)
	return nil
}

func admissionFullPathname(tls *libc.TLS, pVFS, name uintptr, capacity int32, out uintptr) int32 {
	admissionVFSState.Lock()
	v := admissionVFSState.vfs[pVFS]
	admissionVFSState.Unlock()
	if v == nil || name == 0 {
		return sqlite3.SQLITE_CANTOPEN
	}
	path := libc.GoString(name)
	if _, ok := v.allowed[path]; !ok || len(path)+1 > int(capacity) {
		return sqlite3.SQLITE_CANTOPEN
	}
	libc.Xmemcpy(tls, out, name, libc.Tsize_t(len(path)+1))
	return sqlite3.SQLITE_OK
}

func admissionDelete(*libc.TLS, uintptr, uintptr, int32) int32 { return sqlite3.SQLITE_READONLY }

func admissionOpen(tls *libc.TLS, pVFS, zName, pFile uintptr, flags int32, out uintptr) int32 {
	admissionVFSState.Lock()
	v := admissionVFSState.vfs[pVFS]
	admissionVFSState.Unlock()
	if v == nil || zName == 0 {
		return sqlite3.SQLITE_CANTOPEN
	}
	path := libc.GoString(zName)
	approved, ok := v.allowed[path]
	if !ok || flags&(sqlite3.SQLITE_OPEN_MAIN_JOURNAL|sqlite3.SQLITE_OPEN_TEMP_DB|sqlite3.SQLITE_OPEN_TEMP_JOURNAL|sqlite3.SQLITE_OPEN_TRANSIENT_DB|sqlite3.SQLITE_OPEN_SUBJOURNAL|sqlite3.SQLITE_OPEN_SUPER_JOURNAL) != 0 {
		v.deniedOpens.Add(1)
		return sqlite3.SQLITE_CANTOPEN
	}
	flags &^= sqlite3.SQLITE_OPEN_CREATE | sqlite3.SQLITE_OPEN_DELETEONCLOSE
	if !v.writable {
		flags &^= sqlite3.SQLITE_OPEN_READWRITE
		flags |= sqlite3.SQLITE_OPEN_READONLY
	}
	if v.writable {
		return admissionOpenDestination(tls, v, pFile, flags, out, approved)
	}
	base := &v.native
	open := *(*func(*libc.TLS, uintptr, uintptr, uintptr, int32, uintptr) int32)(unsafe.Pointer(&base.FxOpen))
	v.opens.Add(1)
	rc := open(tls, v.base, zName, pFile, flags, out)
	if rc != sqlite3.SQLITE_OK {
		return rc
	}
	original := loadAdmissionC[sqlite3.Tsqlite3_file](tls, pFile).FpMethods
	native := loadAdmissionC[sqlite3.Tsqlite3_io_methods](tls, original)
	closeFile := *(*func(*libc.TLS, uintptr) int32)(unsafe.Pointer(&native.FxClose))
	if !admissionDescriptorMatches(loadAdmissionC[sqlite3.TunixFile](tls, pFile).Fh, approved) {
		closeFile(tls, pFile)
		return sqlite3.SQLITE_CANTOPEN
	}
	f := &admissionFile{vfs: v, original: original, originalMethods: native, table: native}
	methods := &f.table
	methods.FxClose = callbackPointer(admissionClose)
	methods.FxRead = callbackPointer(admissionRead)
	methods.FxWrite = callbackPointer(admissionWrite)
	methods.FxTruncate = callbackPointer(admissionTruncate)
	methods.FxFileControl = callbackPointer(admissionFileControl)
	methods.FxShmMap = callbackPointer(admissionShmMap)
	methods.FxShmUnmap = callbackPointer(admissionShmUnmap)
	methods.FxFetch = 0
	methods.FxUnfetch = 0
	f.pin.Pin(methods)
	f.methods = libc.Xmalloc(tls, libc.Tsize_t(unsafe.Sizeof(f.table)))
	if f.methods == 0 {
		f.pin.Unpin()
		closeFile(tls, pFile)
		return sqlite3.SQLITE_NOMEM
	}
	writeAdmissionC(tls, f.methods, f.table)
	admissionVFSState.Lock()
	v.activeDescriptors.Add(1)
	admissionVFSState.files[pFile] = f
	admissionVFSState.Unlock()
	setAdmissionMethods(tls, pFile, f.methods)
	return rc
}

func admissionDescriptorMatches(fd int32, expected os.FileInfo) bool {
	dup, err := unix.Dup(int(fd))
	if err != nil {
		return false
	}
	f := os.NewFile(uintptr(dup), "admission-descriptor")
	info, err := f.Stat()
	closeErr := f.Close()
	return err == nil && closeErr == nil && info.Mode().IsRegular() && os.SameFile(expected, info)
}

func admissionLookup(p uintptr) (*admissionFile, *sqlite3.Tsqlite3_io_methods) {
	admissionVFSState.Lock()
	f := admissionVFSState.files[p]
	admissionVFSState.Unlock()
	return f, &f.originalMethods
}

func admissionClose(tls *libc.TLS, p uintptr) int32 {
	f, m := admissionLookup(p)
	rc := int32(sqlite3.SQLITE_OK)
	if f.vfs.heldFile == nil {
		closeFile := *(*func(*libc.TLS, uintptr) int32)(unsafe.Pointer(&m.FxClose))
		setAdmissionMethods(tls, p, f.original)
		rc = closeFile(tls, p)
	} else {
		setAdmissionMethods(tls, p, 0)
	}
	admissionVFSState.Lock()
	delete(admissionVFSState.files, p)
	admissionVFSState.Unlock()
	f.pin.Unpin()
	libc.Xfree(tls, f.methods)
	f.vfs.activeDescriptors.Add(-1)
	return rc
}

func admissionRead(tls *libc.TLS, p, b uintptr, n int32, offset int64) int32 {
	_, m := admissionLookup(p)
	f, _ := admissionLookup(p)
	if f.vfs.heldFile != nil {
		return admissionReadDestination(tls, f.vfs, b, n, offset)
	}
	read := *(*func(*libc.TLS, uintptr, uintptr, int32, int64) int32)(unsafe.Pointer(&m.FxRead))
	return read(tls, p, b, n, offset)
}
func admissionWrite(tls *libc.TLS, p, b uintptr, n int32, offset int64) int32 {
	f, m := admissionLookup(p)
	if !f.vfs.writable {
		return sqlite3.SQLITE_READONLY
	}
	if f.vfs.context != nil && f.vfs.context.Err() != nil {
		return sqlite3.SQLITE_INTERRUPT
	}
	if n < 0 || offset < 0 || int64(n) > f.vfs.limit-offset || offset > f.vfs.limit {
		f.vfs.deniedWrites.Add(1)
		return sqlite3.SQLITE_FULL
	}
	f.vfs.writeCalls.Add(1)
	f.vfs.writeBytes.Add(int64(n))
	end := offset + int64(n)
	for old := f.vfs.maxWriteEnd.Load(); end > old; old = f.vfs.maxWriteEnd.Load() {
		if f.vfs.maxWriteEnd.CompareAndSwap(old, end) {
			break
		}
	}
	if f.vfs.heldFile != nil {
		return admissionWriteDestination(tls, f.vfs, b, n, offset)
	}
	write := *(*func(*libc.TLS, uintptr, uintptr, int32, int64) int32)(unsafe.Pointer(&m.FxWrite))
	return write(tls, p, b, n, offset)
}
func admissionTruncate(tls *libc.TLS, p uintptr, n int64) int32 {
	f, m := admissionLookup(p)
	if !f.vfs.writable {
		return sqlite3.SQLITE_READONLY
	}
	if f.vfs.context != nil && f.vfs.context.Err() != nil {
		return sqlite3.SQLITE_INTERRUPT
	}
	if n < 0 || n > f.vfs.limit {
		return sqlite3.SQLITE_FULL
	}
	if f.vfs.heldFile != nil {
		if err := f.vfs.heldFile.Truncate(n); err != nil {
			return sqlite3.SQLITE_IOERR_TRUNCATE
		}
		return sqlite3.SQLITE_OK
	}
	truncate := *(*func(*libc.TLS, uintptr, int64) int32)(unsafe.Pointer(&m.FxTruncate))
	return truncate(tls, p, n)
}
func admissionFileControl(tls *libc.TLS, p uintptr, op int32, arg uintptr) int32 {
	// Allocation hints and unknown controls never reach the filesystem. Query
	// controls not implemented here retain SQLite's documented NOTFOUND fallback.
	return sqlite3.SQLITE_NOTFOUND
}
func admissionShmMap(tls *libc.TLS, p uintptr, region, size, extend int32, out uintptr) int32 {
	f, m := admissionLookup(p)
	if f.vfs.writable || m.FxShmMap == 0 {
		return sqlite3.SQLITE_READONLY
	}
	native := loadAdmissionC[sqlite3.TunixFile](tls, p)
	path := libc.GoString(native.FzPath) + "-shm"
	expected, ok := f.vfs.allowed[path]
	if !ok {
		return sqlite3.SQLITE_READONLY
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(expected, info) {
		return sqlite3.SQLITE_READONLY
	}
	mapFile := *(*func(*libc.TLS, uintptr, int32, int32, int32, uintptr) int32)(unsafe.Pointer(&m.FxShmMap))
	rc := mapFile(tls, p, region, size, 0, out)
	if rc != sqlite3.SQLITE_OK && rc != sqlite3.SQLITE_READONLY {
		return rc
	}
	native = loadAdmissionC[sqlite3.TunixFile](tls, p)
	if native.FpShm == 0 {
		return sqlite3.SQLITE_READONLY
	}
	shm := loadAdmissionC[sqlite3.TunixShm](tls, native.FpShm)
	node := loadAdmissionC[sqlite3.TunixShmNode](tls, shm.FpShmNode)
	if node.FisReadonly == 0 || !admissionDescriptorMatches(node.FhShm, expected) {
		return sqlite3.SQLITE_READONLY
	}
	return rc
}
func admissionShmUnmap(tls *libc.TLS, p uintptr, deleteFlag int32) int32 {
	_, m := admissionLookup(p)
	unmap := *(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&m.FxShmUnmap))
	return unmap(tls, p, 0)
}

func (v *admissionVFS) copyStats(stats *AdmissionCopyStats) {
	stats.DestinationWriteCalls = int(v.writeCalls.Load())
	stats.DestinationWriteBytes = v.writeBytes.Load()
	stats.DestinationMaxWriteEnd = v.maxWriteEnd.Load()
	stats.ProhibitedWrites = int(v.deniedWrites.Load())
	stats.DestinationOpens = int(v.opens.Load())
	stats.ProhibitedOpens = int(v.deniedOpens.Load())
	stats.DestinationOpenDescriptors = int(v.activeDescriptors.Load())
}
