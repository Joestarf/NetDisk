package nfsadapter

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"

	"netdisk/db"
	"netdisk/handlers"
	"netdisk/models"
	"netdisk/storage"
)

var _ billy.Filesystem = (*NetDiskFS)(nil)
var _ billy.Change = (*NetDiskFS)(nil)

// NetDiskFS 将网盘后端映射为 billy.Filesystem。
type NetDiskFS struct {
	ownerID int64
	root    string
}

func NewNetDiskFS(ownerID int64) billy.Filesystem {
	return &NetDiskFS{ownerID: ownerID, root: ""}
}

func (fs *NetDiskFS) Chroot(p string) (billy.Filesystem, error) {
	rel, err := fs.resolvePath(p)
	if err != nil {
		return nil, err
	}
	if rel != "" {
		if _, err := fs.findFolderIDByPath(rel); err != nil {
			return nil, err
		}
	}
	return &NetDiskFS{ownerID: fs.ownerID, root: rel}, nil
}

func (fs *NetDiskFS) Root() string {
	if fs.root == "" {
		return "/"
	}
	return "/" + fs.root
}

func (fs *NetDiskFS) Join(elem ...string) string {
	return path.Join(elem...)
}

func (fs *NetDiskFS) Create(filename string) (billy.File, error) {
	return fs.OpenFile(filename, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o644)
}

func (fs *NetDiskFS) Open(filename string) (billy.File, error) {
	return fs.OpenFile(filename, os.O_RDONLY, 0)
}

func (fs *NetDiskFS) OpenFile(filename string, flag int, _ os.FileMode) (billy.File, error) {
	logicalPath, err := fs.resolvePath(filename)
	if err != nil {
		return nil, err
	}
	if logicalPath == "" {
		return nil, os.ErrPermission
	}

	parentPath := path.Dir(logicalPath)
	if parentPath == "." {
		parentPath = ""
	}
	base := path.Base(logicalPath)

	parentID, err := fs.findFolderIDByPath(parentPath)
	if err != nil {
		return nil, err
	}

	existing, err := handlers.GetFileByNameForOwner(fs.ownerID, parentID, base)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	isWrite := flag&(os.O_WRONLY|os.O_RDWR) != 0
	if !isWrite {
		if existing == nil {
			return nil, os.ErrNotExist
		}
		return fs.openReadOnlyFile(existing)
	}

	if existing == nil && flag&os.O_CREATE == 0 {
		return nil, os.ErrNotExist
	}
	if existing != nil && flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0 {
		return nil, os.ErrExist
	}

	tmp, err := os.CreateTemp("", "nfs-upload-*")
	if err != nil {
		return nil, err
	}

	if existing != nil && flag&os.O_TRUNC == 0 {
		if err := fs.copyRecordToWriter(existing, tmp); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			return nil, err
		}
	}

	if flag&os.O_APPEND != 0 {
		if _, err := tmp.Seek(0, io.SeekEnd); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			return nil, err
		}
	} else {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			return nil, err
		}
	}

	return &uploadFile{
		osBillyFile: &osBillyFile{File: tmp},
		ownerID:     fs.ownerID,
		parentID:    parentID,
		name:        base,
		tmpPath:     tmp.Name(),
		hasChanges:  true,
	}, nil
}

func (fs *NetDiskFS) ReadDir(p string) ([]os.FileInfo, error) {
	logicalPath, err := fs.resolvePath(p)
	if err != nil {
		return nil, err
	}
	folderID, err := fs.findFolderIDByPath(logicalPath)
	if err != nil {
		return nil, err
	}
	children, err := handlers.ListChildrenForOwner(fs.ownerID, folderID)
	if err != nil {
		return nil, err
	}

	infos := make([]os.FileInfo, 0, len(children))
	for _, child := range children {
		mode := os.FileMode(0o644)
		if child.IsFolder {
			mode = os.ModeDir | 0o755
		}
		infos = append(infos, &fileInfo{
			name:    child.Name,
			size:    child.SizeBytes,
			mode:    mode,
			modTime: child.CreatedAt,
			isDir:   child.IsFolder,
		})
	}
	return infos, nil
}

func (fs *NetDiskFS) MkdirAll(filename string, _ os.FileMode) error {
	logicalPath, err := fs.resolvePath(filename)
	if err != nil {
		return err
	}
	if logicalPath == "" {
		return nil
	}

	parts := splitPath(logicalPath)
	var parentID *int64
	for _, seg := range parts {
		folder, err := handlers.GetFolderByNameForOwner(fs.ownerID, parentID, seg)
		if err == nil {
			id := folder.ID
			parentID = &id
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		created, err := handlers.CreateFolderForOwner(fs.ownerID, parentID, seg)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				folder, lookupErr := handlers.GetFolderByNameForOwner(fs.ownerID, parentID, seg)
				if lookupErr != nil {
					return lookupErr
				}
				id := folder.ID
				parentID = &id
				continue
			}
			return err
		}
		id := created.ID
		parentID = &id
	}
	return nil
}

func (fs *NetDiskFS) Remove(filename string) error {
	logicalPath, err := fs.resolvePath(filename)
	if err != nil {
		return err
	}
	if logicalPath == "" {
		return os.ErrPermission
	}

	parentPath := path.Dir(logicalPath)
	if parentPath == "." {
		parentPath = ""
	}
	base := path.Base(logicalPath)
	parentID, err := fs.findFolderIDByPath(parentPath)
	if err != nil {
		return err
	}

	if fileRec, err := handlers.GetFileByNameForOwner(fs.ownerID, parentID, base); err == nil {
		return handlers.DeleteFileForOwner(fs.ownerID, fileRec.ID)
	}

	folder, err := handlers.GetFolderByNameForOwner(fs.ownerID, parentID, base)
	if err != nil {
		return os.ErrNotExist
	}
	return handlers.DeleteFolderForOwner(fs.ownerID, folder.ID)
}

func (fs *NetDiskFS) Rename(oldpath, newpath string) error {
	oldRel, err := fs.resolvePath(oldpath)
	if err != nil {
		return err
	}
	newRel, err := fs.resolvePath(newpath)
	if err != nil {
		return err
	}
	if oldRel == "" || newRel == "" {
		return os.ErrPermission
	}

	oldParentPath, oldBase := splitParentBase(oldRel)
	newParentPath, newBase := splitParentBase(newRel)

	oldParentID, err := fs.findFolderIDByPath(oldParentPath)
	if err != nil {
		return err
	}
	newParentID, err := fs.findFolderIDByPath(newParentPath)
	if err != nil {
		return err
	}

	if fileRec, err := handlers.GetFileByNameForOwner(fs.ownerID, oldParentID, oldBase); err == nil {
		if !sameParent(oldParentID, newParentID) {
			if err := handlers.MoveFileForOwner(fs.ownerID, fileRec.ID, newParentID); err != nil {
				return err
			}
		}
		if oldBase != newBase {
			if err := handlers.RenameFileForOwner(fs.ownerID, fileRec.ID, newBase); err != nil {
				return err
			}
		}
		return nil
	}

	folder, err := handlers.GetFolderByNameForOwner(fs.ownerID, oldParentID, oldBase)
	if err != nil {
		return os.ErrNotExist
	}
	if !sameParent(oldParentID, newParentID) {
		if err := handlers.MoveFolderForOwner(fs.ownerID, folder.ID, newParentID); err != nil {
			return err
		}
	}
	if oldBase != newBase {
		if err := handlers.RenameFolderForOwner(fs.ownerID, folder.ID, newBase); err != nil {
			return err
		}
	}
	return nil
}

func (fs *NetDiskFS) Stat(filename string) (os.FileInfo, error) {
	logicalPath, err := fs.resolvePath(filename)
	if err != nil {
		return nil, err
	}
	if logicalPath == "" {
		return &fileInfo{name: "/", mode: os.ModeDir | 0o755, modTime: time.Now(), isDir: true}, nil
	}

	parentPath, base := splitParentBase(logicalPath)
	parentID, err := fs.findFolderIDByPath(parentPath)
	if err != nil {
		return nil, err
	}

	if fileRec, err := handlers.GetFileByNameForOwner(fs.ownerID, parentID, base); err == nil {
		return &fileInfo{name: fileRec.Name, size: fileRec.SizeBytes, mode: 0o644, modTime: fileRec.CreatedAt}, nil
	}

	folderRec, err := handlers.GetFolderByNameForOwner(fs.ownerID, parentID, base)
	if err == nil {
		return &fileInfo{name: folderRec.Name, mode: os.ModeDir | 0o755, modTime: folderRec.CreatedAt, isDir: true}, nil
	}

	return nil, os.ErrNotExist
}

func (fs *NetDiskFS) Lstat(filename string) (os.FileInfo, error) {
	return fs.Stat(filename)
}

func (fs *NetDiskFS) Symlink(_, _ string) error {
	return billy.ErrNotSupported
}

func (fs *NetDiskFS) Readlink(_ string) (string, error) {
	return "", billy.ErrNotSupported
}

func (fs *NetDiskFS) TempFile(dir, prefix string) (billy.File, error) {
	f, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return nil, err
	}
	return &osBillyFile{File: f}, nil
}

// Chmod/Lchown/Chown/Chtimes 作为 NFS 属性变更兼容实现。
// NetDisk 当前不维护 Unix 权限和 UID/GID，因此这里做 no-op。
func (fs *NetDiskFS) Chmod(_ string, _ os.FileMode) error {
	return nil
}

func (fs *NetDiskFS) Lchown(_ string, _, _ int) error {
	return nil
}

func (fs *NetDiskFS) Chown(_ string, _, _ int) error {
	return nil
}

func (fs *NetDiskFS) Chtimes(_ string, _ time.Time, _ time.Time) error {
	return nil
}

func (fs *NetDiskFS) resolvePath(p string) (string, error) {
	clean := path.Clean("/" + strings.TrimSpace(p))
	combined := path.Clean(path.Join("/", fs.root, clean))
	if !strings.HasPrefix(combined, "/") {
		return "", billy.ErrCrossedBoundary
	}
	if fs.root != "" {
		rootWithSlash := "/" + fs.root
		if combined != rootWithSlash && !strings.HasPrefix(combined, rootWithSlash+"/") {
			return "", billy.ErrCrossedBoundary
		}
		combined = strings.TrimPrefix(combined, rootWithSlash)
		if combined == "" {
			return "", nil
		}
	}
	return strings.TrimPrefix(combined, "/"), nil
}

func (fs *NetDiskFS) findFolderIDByPath(logicalPath string) (*int64, error) {
	logicalPath = strings.TrimSpace(logicalPath)
	if logicalPath == "" || logicalPath == "." {
		return nil, nil
	}
	segments := splitPath(logicalPath)
	var parentID *int64
	for _, seg := range segments {
		folder, err := handlers.GetFolderByNameForOwner(fs.ownerID, parentID, seg)
		if err != nil {
			return nil, os.ErrNotExist
		}
		id := folder.ID
		parentID = &id
	}
	return parentID, nil
}

func (fs *NetDiskFS) openReadOnlyFile(rec *models.FileRecord) (billy.File, error) {
	f, err := os.Open(rec.DiskPath)
	if err == nil {
		return &readOnlyFile{File: f, displayName: rec.Name}, nil
	}

	if rec.BlobHash == nil || strings.TrimSpace(*rec.BlobHash) == "" {
		return nil, err
	}
	blob, blobErr := db.GetBlobByHash(*rec.BlobHash)
	if blobErr != nil || !strings.EqualFold(blob.StorageBackend, "oss") || strings.TrimSpace(blob.StorageKey) == "" {
		return nil, err
	}
	backend := storage.GetObjectBackend()
	if backend == nil {
		return nil, err
	}
	signedURL, urlErr := backend.GetDownloadURL(blob.StorageKey, rec.Name)
	if urlErr != nil {
		return nil, urlErr
	}
	resp, httpErr := http.Get(signedURL)
	if httpErr != nil {
		return nil, httpErr
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("download object failed: status=%d", resp.StatusCode)
	}

	tmp, tmpErr := os.CreateTemp("", "nfs-read-*")
	if tmpErr != nil {
		return nil, tmpErr
	}
	if _, copyErr := io.Copy(tmp, resp.Body); copyErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, copyErr
	}
	if _, seekErr := tmp.Seek(0, io.SeekStart); seekErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, seekErr
	}
	return &readOnlyFile{File: tmp, displayName: rec.Name, removeOnClose: true}, nil
}

func (fs *NetDiskFS) copyRecordToWriter(rec *models.FileRecord, w io.Writer) error {
	f, err := fs.openReadOnlyFile(rec)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func splitPath(p string) []string {
	clean := path.Clean("/" + p)
	if clean == "/" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	res := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			res = append(res, part)
		}
	}
	return res
}

func splitParentBase(p string) (string, string) {
	parent := path.Dir(p)
	if parent == "." {
		parent = ""
	}
	return parent, path.Base(p)
}

func sameParent(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

type osBillyFile struct {
	*os.File
}

func (f *osBillyFile) Lock() error   { return nil }
func (f *osBillyFile) Unlock() error { return nil }

type readOnlyFile struct {
	*os.File
	displayName   string
	removeOnClose bool
}

func (f *readOnlyFile) Name() string {
	if f.displayName != "" {
		return f.displayName
	}
	return f.File.Name()
}

func (f *readOnlyFile) Write(_ []byte) (int, error) { return 0, os.ErrPermission }
func (f *readOnlyFile) Truncate(_ int64) error      { return os.ErrPermission }
func (f *readOnlyFile) Lock() error                 { return nil }
func (f *readOnlyFile) Unlock() error               { return nil }

func (f *readOnlyFile) Close() error {
	name := f.File.Name()
	err := f.File.Close()
	if f.removeOnClose {
		_ = os.Remove(name)
	}
	return err
}

type uploadFile struct {
	*osBillyFile
	ownerID    int64
	parentID   *int64
	name       string
	tmpPath    string
	hasChanges bool
	closed     bool
}

func (f *uploadFile) Lock() error   { return nil }
func (f *uploadFile) Unlock() error { return nil }

func (f *uploadFile) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true

	if f.hasChanges {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			_ = f.File.Close()
			_ = os.Remove(f.tmpPath)
			return err
		}
		if _, err := handlers.SaveFileForOwner(f.ownerID, f.parentID, f.name, f.File); err != nil {
			_ = f.File.Close()
			_ = os.Remove(f.tmpPath)
			return err
		}
	}

	err := f.File.Close()
	_ = os.Remove(f.tmpPath)
	return err
}

type fileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	isDir   bool
}

func (fi *fileInfo) Name() string       { return fi.name }
func (fi *fileInfo) Size() int64        { return fi.size }
func (fi *fileInfo) Mode() os.FileMode  { return fi.mode }
func (fi *fileInfo) ModTime() time.Time { return fi.modTime }
func (fi *fileInfo) IsDir() bool        { return fi.isDir }
func (fi *fileInfo) Sys() interface{}   { return nil }
