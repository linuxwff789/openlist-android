// Package openlistbridge provides cloud drive operations for Android via gomobile.
package openlistbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	_ "github.com/OpenListTeam/OpenList/v4/drivers/189"
	_ "github.com/OpenListTeam/OpenList/v4/drivers/aliyundrive_open"
	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	_ "github.com/OpenListTeam/OpenList/v4/drivers/webdav"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/google/uuid"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func init() {
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "tcp", "8.8.8.8:53")
		},
	}
}

func ensureInit() (err error) {
	if base.RestyClient != nil {
		return nil
	}
	if conf.Conf == nil {
		conf.Conf = &conf.Config{TlsInsecureSkipVerify: false}
	}
	base.InitClient()
	// Initialize in-memory GORM/SQLite — required by op.MustSaveDriverStorage
	var sqlDB *gorm.DB
	sqlDB, err = gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	// db.Init sets global db var + runs AutoMigrate (no longer calls log.Fatalf)
	if err = db.Init(sqlDB); err != nil {
		return fmt.Errorf("db init: %w", err)
	}
	return nil
}

var (
	instMu sync.RWMutex
	insts  = make(map[string]driver.Driver)
)

type cloudFile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	IsDir     bool   `json:"is_dir"`
	Modified  int64  `json:"modified_at"`
	Path      string `json:"path"`
	Thumbnail string `json:"thumbnail,omitempty"`
}

func objToCloudFile(obj model.Obj) cloudFile {
	cf := cloudFile{
		ID:       obj.GetID(),
		Name:     obj.GetName(),
		Size:     obj.GetSize(),
		IsDir:    obj.IsDir(),
		Modified: obj.ModTime().UnixMilli(),
		Path:     obj.GetPath(),
	}
	if thumb, ok := obj.(model.Thumb); ok {
		cf.Thumbnail = thumb.Thumb()
	}
	return cf
}

func newHandle() string { return uuid.New().String() }

func resultJSON(data interface{}) string {
	b, _ := json.Marshal(map[string]interface{}{"success": true, "data": data})
	return string(b)
}

func errorJSON(err error) string {
	b, _ := json.Marshal(map[string]interface{}{"success": false, "error": err.Error()})
	return string(b)
}

// ── Exported API ──

func Create(driverType, configJSON string) (str string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC in Create:\n%s", string(debug.Stack()))
			str = errorJSON(fmt.Errorf("PANIC: %v", r))
		}
	}()
	if err := ensureInit(); err != nil {
		return errorJSON(fmt.Errorf("init: %w", err))
	}
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

func List(handle, path string) (str string) {
	defer func() {
		if r := recover(); r != nil {
			str = errorJSON(fmt.Errorf("PANIC: %v", r))
		}
	}()
	drv, err := getDrv(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dir, err := resolveDir(ctx, drv, path)
	if err != nil {
		return errorJSON(err)
	}
	files, err := drv.List(ctx, dir, model.ListArgs{})
	if err != nil {
		return errorJSON(err)
	}
	result := make([]cloudFile, 0, len(files))
	for _, f := range files {
		result = append(result, objToCloudFile(f))
	}
	return resultJSON(result)
}

func GetDownloadURL(handle, path string) (str string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC in GetDownloadURL:\n%s", string(debug.Stack()))
			str = errorJSON(fmt.Errorf("PANIC: %v", r))
		}
	}()
	drv, err := getDrv(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	obj, err := getObj(ctx, drv, path)
	if err != nil {
		return errorJSON(err)
	}
	if obj.IsDir() {
		return errorJSON(fmt.Errorf("cannot get download url for a directory"))
	}
	link, err := drv.Link(ctx, obj, model.LinkArgs{})
	if err != nil {
		return errorJSON(err)
	}
	return resultJSON(map[string]string{"url": link.URL})
}

func Upload(handle, parentPath, fileName, localFilePath, mimeType string) (str string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC in Upload:\n%s", string(debug.Stack()))
			str = errorJSON(fmt.Errorf("PANIC: %v", r))
		}
	}()
	drv, err := getDrv(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	parentDir, err := resolveDir(ctx, drv, parentPath)
	if err != nil {
		return errorJSON(err)
	}
	log.Printf("upload: parentPath=%q parentID=%q fileName=%q fileSize=%d",
		parentPath, parentDir.GetID(), fileName, fileSize)
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
		Obj: &model.Object{Name: fileName, Size: fileSize},
		Reader:   io.LimitReader(f, fileSize),
		Mimetype: mimeType,
	}
	if putResult, ok := drv.(driver.PutResult); ok {
		obj, err := putResult.Put(ctx, parentDir, fileStream, func(pct float64) {})
		if err != nil {
			return errorJSON(err)
		}
		if obj != nil {
			return resultJSON(objToCloudFile(obj))
		}
		return resultJSON(map[string]string{"name": fileName})
	}

	if putter, ok := drv.(driver.Put); ok {
		err = putter.Put(ctx, parentDir, fileStream, func(pct float64) {})
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(map[string]string{"name": fileName})
	}
	return errorJSON(fmt.Errorf("driver does not support upload"))
}

func Mkdir(handle, parentPath, dirName string) (str string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC in Mkdir:\n%s", string(debug.Stack()))
			str = errorJSON(fmt.Errorf("PANIC: %v", r))
		}
	}()
	drv, err := getDrv(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	parentDir, err := resolveDir(ctx, drv, parentPath)
	if err != nil {
		return errorJSON(err)
	}
	if mkdirResult, ok := drv.(driver.MkdirResult); ok {
		obj, err := mkdirResult.MakeDir(ctx, parentDir, dirName)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(objToCloudFile(obj))
	}
	if mkdir, ok := drv.(driver.Mkdir); ok {
		err = mkdir.MakeDir(ctx, parentDir, dirName)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(map[string]string{"name": dirName})
	}
	return errorJSON(fmt.Errorf("driver does not support mkdir"))
}

func Delete(handle, path string) (str string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC in Delete:\n%s", string(debug.Stack()))
			str = errorJSON(fmt.Errorf("PANIC: %v", r))
		}
	}()
	drv, err := getDrv(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	obj, err := getObj(ctx, drv, path)
	if err != nil {
		return errorJSON(err)
	}
	remover, ok := drv.(driver.Remove)
	if !ok {
		return errorJSON(fmt.Errorf("driver does not support delete"))
	}
	err = remover.Remove(ctx, obj)
	if err != nil {
		return errorJSON(err)
	}
	return resultJSON(map[string]string{"status": "deleted"})
}

func Rename(handle, path, newName string) (str string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC in Rename:\n%s", string(debug.Stack()))
			str = errorJSON(fmt.Errorf("PANIC: %v", r))
		}
	}()
	drv, err := getDrv(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	obj, err := getObj(ctx, drv, path)
	if err != nil {
		return errorJSON(err)
	}
	if renameResult, ok := drv.(driver.RenameResult); ok {
		newObj, err := renameResult.Rename(ctx, obj, newName)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(objToCloudFile(newObj))
	}
	if renamer, ok := drv.(driver.Rename); ok {
		err = renamer.Rename(ctx, obj, newName)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(map[string]string{"name": newName})
	}
	return errorJSON(fmt.Errorf("driver does not support rename"))
}

func Move(handle, srcPath, dstDirPath string) string {
	drv, err := getDrv(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srcObj, err := getObj(ctx, drv, srcPath)
	if err != nil {
		return errorJSON(err)
	}
	dstDir, err := resolveDir(ctx, drv, dstDirPath)
	if err != nil {
		return errorJSON(err)
	}
	if moveResult, ok := drv.(driver.MoveResult); ok {
		newObj, err := moveResult.Move(ctx, srcObj, dstDir)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(objToCloudFile(newObj))
	}
	if mover, ok := drv.(driver.Move); ok {
		err = mover.Move(ctx, srcObj, dstDir)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(map[string]string{"status": "moved"})
	}
	return errorJSON(fmt.Errorf("driver does not support move"))
}

func Copy(handle, srcPath, dstDirPath string) string {
	drv, err := getDrv(handle)
	if err != nil {
		return errorJSON(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srcObj, err := getObj(ctx, drv, srcPath)
	if err != nil {
		return errorJSON(err)
	}
	dstDir, err := resolveDir(ctx, drv, dstDirPath)
	if err != nil {
		return errorJSON(err)
	}
	if copyResult, ok := drv.(driver.CopyResult); ok {
		newObj, err := copyResult.Copy(ctx, srcObj, dstDir)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(objToCloudFile(newObj))
	}
	if copier, ok := drv.(driver.Copy); ok {
		err = copier.Copy(ctx, srcObj, dstDir)
		if err != nil {
			return errorJSON(err)
		}
		return resultJSON(map[string]string{"status": "copied"})
	}
	return errorJSON(fmt.Errorf("driver does not support copy"))
}

func Destroy(handle string) (str string) {
	defer func() {
		if r := recover(); r != nil {
			str = errorJSON(fmt.Errorf("PANIC: %v", r))
		}
	}()
	instMu.Lock()
	drv, ok := insts[handle]
	if ok {
		delete(insts, handle)
	}
	instMu.Unlock()
	if ok {
		_ = drv.Drop(context.Background())
	}
	return resultJSON(map[string]string{"status": "destroyed"})
}

// ── Internal helpers (from bridge.go) ──

func getDrv(h string) (driver.Driver, error) {
	instMu.RLock()
	drv, ok := insts[h]
	instMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("invalid handle: %s", h)
	}
	return drv, nil
}

func initStorage(ctx context.Context, driverName, additionJSON string) (driver.Driver, error) {
	ensureInit()
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
	log.Printf("initStorage: driver=%q driverNew=%v storageDriver=%v addition=%v resty=%v",
		driverName, driverNew, storageDriver, storageDriver.GetAddition(), base.RestyClient)
	if err := storageDriver.Init(ctx); err != nil {
		_ = storageDriver.Drop(ctx)
		return nil, fmt.Errorf("init: %w", err)
	}
	return storageDriver, nil
}

func rootObj(d driver.Driver) model.Obj {
	if getter, ok := d.(driver.GetRooter); ok {
		ctx := context.Background()
		if root, err := getter.GetRoot(ctx); err == nil {
			return root
		}
	}
	s := d.GetStorage()
	return &model.Object{
		ID:       s.MountPath,
		Path:     s.MountPath,
		Name:     "root",
		Modified: s.Modified,
		IsFolder: true,
	}
}

func getObj(ctx context.Context, d driver.Driver, path string) (model.Obj, error) {
	if path == "" || path == "/" {
		return rootObj(d), nil
	}
	if getter, ok := d.(driver.Getter); ok {
		obj, err := getter.Get(ctx, path)
		if err == nil {
			return obj, nil
		}
		if !errs.IsObjectNotFound(err) {
			return nil, err
		}
	}
	return walkTo(ctx, d, path)
}

func resolveDir(ctx context.Context, d driver.Driver, path string) (model.Obj, error) {
	if path == "" || path == "/" {
		return rootObj(d), nil
	}
	if getter, ok := d.(driver.Getter); ok {
		obj, err := getter.Get(ctx, path)
		if err == nil && obj.IsDir() {
			return obj, nil
		}
	}
	return walkTo(ctx, d, path)
}

func walkTo(ctx context.Context, d driver.Driver, path string) (model.Obj, error) {
	if path == "" || path == "/" {
		return rootObj(d), nil
	}
	parts := splitAndClean(path)
	return walkParts(ctx, d, rootObj(d), parts)
}

func walkParts(ctx context.Context, d driver.Driver, dir model.Obj, parts []string) (model.Obj, error) {
	if len(parts) == 0 {
		return dir, nil
	}
	files, err := d.List(ctx, dir, model.ListArgs{})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir.GetPath(), err)
	}
	for _, f := range files {
		if f.GetName() == parts[0] {
			if len(parts) == 1 {
				return f, nil
			}
			if !f.IsDir() {
				return nil, fmt.Errorf("not a directory: %s", parts[0])
			}
			return walkParts(ctx, d, f, parts[1:])
		}
	}
	return nil, fmt.Errorf("path not found: %s", parts[0])
}

func splitAndClean(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}
