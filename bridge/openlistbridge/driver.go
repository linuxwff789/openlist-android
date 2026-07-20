// Package openlistbridge provides cloud drive operations for Android via gomobile.
package openlistbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	_ "github.com/OpenListTeam/OpenList/v4/drivers/189"
	_ "github.com/OpenListTeam/OpenList/v4/drivers/aliyundrive_open"
	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	_ "github.com/OpenListTeam/OpenList/v4/drivers/webdav"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/google/uuid"
)

func init() {
	// Use pure Go DNS; Android system DNS may not be available in JNI context.
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "tcp", "8.8.8.8:53")
		},
	}
	// Initialize minimal config for drivers/base HTTP client
	if conf.Conf == nil {
		conf.Conf = &conf.Config{TlsInsecureSkipVerify: false}
	}
	base.InitClient()
}

var (
	instMu sync.RWMutex
	insts  = make(map[string]driver.Driver)
)

type cloudFile struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	IsDir      bool   `json:"is_dir"`
	ModifiedAt int64  `json:"modified_at"`
	Path       string `json:"path"`
	Thumbnail  string `json:"thumbnail,omitempty"`
}

// Create initializes a cloud drive driver and returns a handle string.
// driverType: "AliyundriveOpen", "WebDav", etc.
// configJSON: driver-specific JSON config.
// Returns JSON: {"success":true,"data":{"handle":"..."}} or {"success":false,"error":"..."}
func Create(driverType, configJSON string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h := newHandle()
	drv, err := initStorage(ctx, driverType, configJSON)
	if err != nil {
		return errorJSON(err)
	}

	instMu.Lock()
	insts[h] = drv
	instMu.Unlock()

	return resultJSON(map[string]string{"handle": h})
}

// List returns files in a directory.
// handle: driver handle from Create.
// path: directory path.
// Returns JSON with file list.
func List(handle, path string) string {
	inst, err := getInstance(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if path == "" {
		path = "/"
	}

	dir := rootObj(inst)
	if path != "/" {
		dir, err = resolveDir(ctx, inst, path)
		if err != nil {
			return errorJSON(err)
		}
	}

	files, err := inst.List(ctx, dir, model.ListArgs{})
	if err != nil {
		return errorJSON(err)
	}

	result := make([]cloudFile, 0, len(files))
	for _, f := range files {
		result = append(result, objToCloudFile(f))
	}
	return resultJSON(result)
}

// GetDownloadURL returns a download URL for a file.
func GetDownloadURL(handle, path string) string {
	inst, err := getInstance(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	obj, err := getObj(ctx, inst, path)
	if err != nil {
		return errorJSON(err)
	}
	if obj.IsDir() {
		return errorJSON(fmt.Errorf("cannot get download url for a directory"))
	}

	link, err := inst.Link(ctx, obj, model.LinkArgs{})
	if err != nil {
		return errorJSON(err)
	}
	return resultJSON(map[string]string{"url": link.URL})
}

// Upload uploads a file from local path.
func Upload(handle, parentPath, fileName, localFilePath, mimeType string) string {
	inst, err := getInstance(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	parentDir, err := resolveDir(ctx, inst, parentPath)
	if err != nil {
		return errorJSON(err)
	}

	f, err := os.Open(localFilePath)
	if err != nil {
		return errorJSON(fmt.Errorf("open file: %w", err))
	}
	defer f.Close()

	stat, _ := f.Stat()
	fileSize := int64(0)
	if stat != nil {
		fileSize = stat.Size()
	}

	fileStream := &stream.FileStream{
		Ctx: ctx,
		Obj: &model.Object{
			Name: fileName,
			Size: fileSize,
		},
		Reader:   io.LimitReader(f, fileSize),
		Mimetype: mimeType,
	}

	if putResult, ok := inst.(driver.PutResult); ok {
		obj, err := putResult.Put(ctx, parentDir, fileStream, nil)
		if err != nil {
			return errorJSON(err)
		}
		if obj != nil {
			return resultJSON(objToCloudFile(obj))
		}
		return resultJSON(map[string]string{"name": fileName})
	}

	if putter, ok := inst.(driver.Put); ok {
		err = putter.Put(ctx, parentDir, fileStream, nil)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(map[string]string{"name": fileName})
	}

	return errorJSON(fmt.Errorf("driver does not support upload"))
}

// Mkdir creates a directory.
func Mkdir(handle, parentPath, dirName string) string {
	inst, err := getInstance(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	parentDir, err := resolveDir(ctx, inst, parentPath)
	if err != nil {
		return errorJSON(err)
	}

	if mkdirResult, ok := inst.(driver.MkdirResult); ok {
		obj, err := mkdirResult.MakeDir(ctx, parentDir, dirName)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(objToCloudFile(obj))
	}

	if mkdir, ok := inst.(driver.Mkdir); ok {
		err = mkdir.MakeDir(ctx, parentDir, dirName)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(map[string]string{"name": dirName})
	}

	return errorJSON(fmt.Errorf("driver does not support mkdir"))
}

// Delete removes a file or directory.
func Delete(handle, path string) string {
	inst, err := getInstance(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	obj, err := getObj(ctx, inst, path)
	if err != nil {
		return errorJSON(err)
	}

	remover, ok := inst.(driver.Remove)
	if !ok {
		return errorJSON(fmt.Errorf("driver does not support delete"))
	}
	err = remover.Remove(ctx, obj)
	if err != nil {
		return errorJSON(err)
	}
	return resultJSON(map[string]string{"status": "deleted"})
}

// Rename renames a file or directory.
func Rename(handle, path, newName string) string {
	inst, err := getInstance(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	obj, err := getObj(ctx, inst, path)
	if err != nil {
		return errorJSON(err)
	}

	if renameResult, ok := inst.(driver.RenameResult); ok {
		newObj, err := renameResult.Rename(ctx, obj, newName)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(objToCloudFile(newObj))
	}

	if renamer, ok := inst.(driver.Rename); ok {
		err = renamer.Rename(ctx, obj, newName)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(map[string]string{"name": newName})
	}

	return errorJSON(fmt.Errorf("driver does not support rename"))
}

// Move moves a file or directory.
func Move(handle, srcPath, dstDirPath string) string {
	inst, err := getInstance(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srcObj, err := getObj(ctx, inst, srcPath)
	if err != nil {
		return errorJSON(err)
	}
	dstDir, err := resolveDir(ctx, inst, dstDirPath)
	if err != nil {
		return errorJSON(err)
	}

	if moveResult, ok := inst.(driver.MoveResult); ok {
		newObj, err := moveResult.Move(ctx, srcObj, dstDir)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(objToCloudFile(newObj))
	}

	if mover, ok := inst.(driver.Move); ok {
		err = mover.Move(ctx, srcObj, dstDir)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(map[string]string{"status": "moved"})
	}

	return errorJSON(fmt.Errorf("driver does not support move"))
}

// Copy copies a file or directory.
func Copy(handle, srcPath, dstDirPath string) string {
	inst, err := getInstance(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srcObj, err := getObj(ctx, inst, srcPath)
	if err != nil {
		return errorJSON(err)
	}
	dstDir, err := resolveDir(ctx, inst, dstDirPath)
	if err != nil {
		return errorJSON(err)
	}

	if copyResult, ok := inst.(driver.CopyResult); ok {
		newObj, err := copyResult.Copy(ctx, srcObj, dstDir)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(objToCloudFile(newObj))
	}

	if copier, ok := inst.(driver.Copy); ok {
		err = copier.Copy(ctx, srcObj, dstDir)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(map[string]string{"status": "copied"})
	}

	return errorJSON(fmt.Errorf("driver does not support copy"))
}

// Destroy releases a driver instance.
func Destroy(handle string) string {
	instMu.Lock()
	inst, ok := insts[handle]
	if ok {
		delete(insts, handle)
	}
	instMu.Unlock()

	if ok {
		_ = inst.Drop(context.Background())
	}
	return resultJSON(map[string]string{"status": "destroyed"})
}

// ---- internal helpers ----

func getInstance(h string) (driver.Driver, error) {
	instMu.RLock()
	drv, ok := insts[h]
	instMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("handle not found: %s", h)
	}
	return drv, nil
}

func newHandle() string {
	return uuid.New().String()
}

func initStorage(ctx context.Context, driverName, additionJSON string) (driver.Driver, error) {
	driverNew, err := op.GetDriver(driverName)
	if err != nil {
		return nil, fmt.Errorf("unknown driver %q: %w", driverName, err)
	}

	storage := model.Storage{
		MountPath: "/",
		Driver:    driverName,
		Addition:  additionJSON,
		Modified:  time.Now(),
	}
	storageDriver := driverNew()
	storageDriver.SetStorage(storage)

	if err := utils.Json.UnmarshalFromString(additionJSON, storageDriver.GetAddition()); err != nil {
		return nil, fmt.Errorf("unmarshal addition: %w", err)
	}

	if err := storageDriver.Init(ctx); err != nil {
		_ = storageDriver.Drop(ctx)
		return nil, fmt.Errorf("init: %w", err)
	}

	return storageDriver, nil
}

func rootObj(drv driver.Driver) model.Object {
	storage := drv.GetStorage()
	return model.Object{
		ID:       storage.RootFolder.ID,
		Name:     storage.RootFolder.Name,
		Size:     0,
		IsDir:    true,
		Modified: storage.Modified,
	}
}

func resolveDir(ctx context.Context, drv driver.Driver, dirPath string) (model.Object, error) {
	dir := rootObj(drv)
	if dirPath == "/" {
		return dir, nil
	}
	files, err := drv.List(ctx, dir, model.ListArgs{})
	if err != nil {
		return model.Object{}, err
	}
	parts := splitPath(dirPath)
	for _, part := range parts {
		found := false
		for _, f := range files {
			if f.GetName() == part && f.IsDir() {
				dir = f
				found = true
				break
			}
		}
		if !found {
			return model.Object{}, fmt.Errorf("path not found: %s/%s", dirPath, part)
		}
		files, err = drv.List(ctx, dir, model.ListArgs{})
		if err != nil {
			return model.Object{}, err
		}
	}
	return dir, nil
}

func getObj(ctx context.Context, drv driver.Driver, path string) (model.Object, error) {
	dirPath := "/"
	name := path
	if idx := lastSlash(path); idx >= 0 {
		dirPath = path[:idx]
		name = path[idx+1:]
	}
	dir, err := resolveDir(ctx, drv, dirPath)
	if err != nil {
		return model.Object{}, err
	}
	files, err := drv.List(ctx, dir, model.ListArgs{})
	if err != nil {
		return model.Object{}, err
	}
	for _, f := range files {
		if f.GetName() == name {
			return f, nil
		}
	}
	return model.Object{}, fmt.Errorf("not found: %s", path)
}

func splitPath(p string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			if i > start {
				parts = append(parts, p[start:i])
			}
			start = i + 1
		}
	}
	if start < len(p) {
		parts = append(parts, p[start:])
	}
	return parts
}

func lastSlash(p string) int {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return i
		}
	}
	return -1
}

func objToCloudFile(obj model.Object) cloudFile {
	return cloudFile{
		ID:         obj.GetID(),
		Name:       obj.GetName(),
		Size:       obj.GetSize(),
		IsDir:      obj.IsDir(),
		ModifiedAt: obj.ModifyTime().UnixMilli(),
	}
}

func resultJSON(data interface{}) string {
	b, _ := json.Marshal(map[string]interface{}{
		"success": true,
		"data":    data,
	})
	return string(b)
}

func errorJSON(err error) string {
	b, _ := json.Marshal(map[string]interface{}{
		"success": false,
		"error":   err.Error(),
	})
	return string(b)
}
