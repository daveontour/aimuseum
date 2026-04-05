// Package tusfilestore is a fork of github.com/tus/tusd/v2/pkg/filestore with
// Windows-safe open flags for appending chunks. The upstream store uses
// O_WRONLY|O_APPEND, which can return "Access is denied" on Windows when
// antivirus, Controlled Folder Access, or share modes interact badly with
// WRONLY-append opens. Using O_RDWR|O_APPEND matches common workarounds.
//
// Derived from tusd v2 filestore; upload ID generation matches tus internal/uid.
package tusfilestore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/tus/tusd/v2/pkg/handler"
)

const (
	StorageKeyPath     = "Path"
	StorageKeyInfoPath = "InfoPath"
)

const DefaultDirPerm = 0775
const DefaultFilePerm = 0664

// New returns a FileStore with default Unix permission bits (same as tusd's filestore.New).
func New(path string) FileStore {
	return FileStore{
		Path:         path,
		DirModePerm:  os.FileMode(DefaultDirPerm) & os.ModePerm,
		FileModePerm: os.FileMode(DefaultFilePerm) & os.ModePerm,
	}
}

type FileStore struct {
	Path         string
	DirModePerm  fs.FileMode
	FileModePerm fs.FileMode
}

func randomUploadID() string {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id)
}

func (store FileStore) UseIn(composer *handler.StoreComposer) {
	composer.UseCore(store)
	composer.UseTerminater(store)
	composer.UseConcater(store)
	composer.UseLengthDeferrer(store)
	composer.UseContentServer(store)
}

func (store FileStore) NewUpload(ctx context.Context, info handler.FileInfo) (handler.Upload, error) {
	if info.ID == "" {
		info.ID = randomUploadID()
	}

	infoPath := store.infoPath(info.ID)
	var binPath string
	if info.Storage != nil && info.Storage[StorageKeyPath] != "" {
		if filepath.IsAbs(info.Storage[StorageKeyPath]) {
			binPath = info.Storage[StorageKeyPath]
		} else {
			binPath = filepath.Join(store.Path, info.Storage[StorageKeyPath])
		}
	} else {
		binPath = store.defaultBinPath(info.ID)
	}

	info.Storage = map[string]string{
		"Type":             "filestore",
		StorageKeyPath:     binPath,
		StorageKeyInfoPath: infoPath,
	}

	if err := createFile(binPath, store.DirModePerm, store.FileModePerm, nil); err != nil {
		return nil, err
	}

	upload := &fileUpload{
		info:         info,
		infoPath:     infoPath,
		binPath:      binPath,
		dirModePerm:  store.DirModePerm,
		fileModePerm: store.FileModePerm,
	}

	if err := upload.writeInfo(); err != nil {
		return nil, err
	}

	return upload, nil
}

func (store FileStore) GetUpload(ctx context.Context, id string) (handler.Upload, error) {
	infoPath := store.infoPath(id)
	data, err := os.ReadFile(infoPath)
	if err != nil {
		if os.IsNotExist(err) {
			err = handler.ErrNotFound
		}
		return nil, err
	}
	var info handler.FileInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}

	var binPath string
	if info.Storage != nil && info.Storage[StorageKeyPath] != "" {
		binPath = info.Storage[StorageKeyPath]
	} else {
		binPath = store.defaultBinPath(info.ID)
	}

	stat, err := os.Stat(binPath)
	if err != nil {
		if os.IsNotExist(err) {
			err = handler.ErrNotFound
		}
		return nil, err
	}

	info.Offset = stat.Size()

	return &fileUpload{
		info:         info,
		binPath:      binPath,
		infoPath:     infoPath,
		dirModePerm:  store.DirModePerm,
		fileModePerm: store.FileModePerm,
	}, nil
}

func (store FileStore) AsTerminatableUpload(upload handler.Upload) handler.TerminatableUpload {
	return upload.(*fileUpload)
}

func (store FileStore) AsLengthDeclarableUpload(upload handler.Upload) handler.LengthDeclarableUpload {
	return upload.(*fileUpload)
}

func (store FileStore) AsConcatableUpload(upload handler.Upload) handler.ConcatableUpload {
	return upload.(*fileUpload)
}

func (store FileStore) AsServableUpload(upload handler.Upload) handler.ServableUpload {
	return upload.(*fileUpload)
}

func (store FileStore) defaultBinPath(id string) string {
	return filepath.Join(store.Path, id)
}

func (store FileStore) infoPath(id string) string {
	return filepath.Join(store.Path, id+".info")
}

type fileUpload struct {
	info         handler.FileInfo
	infoPath     string
	binPath      string
	dirModePerm  fs.FileMode
	fileModePerm fs.FileMode
}

func (upload *fileUpload) GetInfo(ctx context.Context) (handler.FileInfo, error) {
	return upload.info, nil
}

func (upload *fileUpload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	file, err := os.OpenFile(upload.binPath, os.O_RDWR|os.O_APPEND, upload.fileModePerm)
	if err != nil {
		return 0, err
	}

	n, err := io.Copy(file, src)
	upload.info.Offset += n
	if err != nil {
		file.Close()
		return n, err
	}

	return n, file.Close()
}

func (upload *fileUpload) GetReader(ctx context.Context) (io.ReadCloser, error) {
	return os.Open(upload.binPath)
}

func (upload *fileUpload) Terminate(ctx context.Context) error {
	err := os.Remove(upload.binPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	err = os.Remove(upload.infoPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

func (upload *fileUpload) ConcatUploads(ctx context.Context, uploads []handler.Upload) (err error) {
	file, err := os.OpenFile(upload.binPath, os.O_RDWR|os.O_APPEND, upload.fileModePerm)
	if err != nil {
		return err
	}
	defer func() {
		cerr := file.Close()
		if err == nil {
			err = cerr
		}
	}()

	for _, partialUpload := range uploads {
		if err := partialUpload.(*fileUpload).appendTo(file); err != nil {
			return err
		}
	}

	return
}

func (upload *fileUpload) appendTo(file *os.File) error {
	src, err := os.Open(upload.binPath)
	if err != nil {
		return err
	}

	if _, err := io.Copy(file, src); err != nil {
		src.Close()
		return err
	}

	return src.Close()
}

func (upload *fileUpload) DeclareLength(ctx context.Context, length int64) error {
	upload.info.Size = length
	upload.info.SizeIsDeferred = false
	return upload.writeInfo()
}

func (upload *fileUpload) writeInfo() error {
	data, err := json.Marshal(upload.info)
	if err != nil {
		return err
	}
	return createFile(upload.infoPath, upload.dirModePerm, upload.fileModePerm, data)
}

func (upload *fileUpload) FinishUpload(ctx context.Context) error {
	return nil
}

func (upload *fileUpload) ServeContent(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	http.ServeFile(w, r, upload.binPath)
	return nil
}

func createFile(path string, dirPerm fs.FileMode, filePerm fs.FileMode, content []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, filePerm)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
				return fmt.Errorf("failed to create directory for %s: %s", path, err)
			}

			file, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, filePerm)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	if content != nil {
		if _, err := file.Write(content); err != nil {
			return err
		}
	}

	return file.Close()
}
