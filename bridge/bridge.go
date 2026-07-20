package main

/*
#include <jni.h>
#include <stdlib.h>

// JNI helper wrappers — Go cgo cannot call (*env)->Method directly.
static inline jstring _go_NewStringUTF(JNIEnv* env, const char* s) {
    return (*env)->NewStringUTF(env, s);
}
static inline const char* _go_GetStringUTFChars(JNIEnv* env, jstring s, jboolean* isCopy) {
    return (*env)->GetStringUTFChars(env, s, isCopy);
}
static inline void _go_ReleaseStringUTFChars(JNIEnv* env, jstring s, const char* c) {
    (*env)->ReleaseStringUTFChars(env, s, c);
}

// Initialize GODEBUG before Go runtime starts.
// Prevents signal handler conflicts between Go and JVM on Android.
__attribute__((constructor)) static void _go_android_init(void) {
    setenv("GODEBUG", "asyncpreemptoff=1,inittrace=0", 1);
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	// Import drivers for their init() registration
	_ "github.com/OpenListTeam/OpenList/v4/drivers/189"
	_ "github.com/OpenListTeam/OpenList/v4/drivers/aliyundrive_open"
	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	_ "github.com/OpenListTeam/OpenList/v4/drivers/webdav"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/google/uuid"
)

// ---- handle management ----

// init runs once during library load: set up Android-compatible DNS
func init() {
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			// Use public DNS; Android system DNS may not be available
			return d.DialContext(ctx, "tcp", "8.8.8.8:53")
		},
	}
}

type instance struct {
	drv     driver.Driver
	drvName string
}

var (
	instMu sync.RWMutex
	insts  = make(map[string]*instance)
)

// ---- JNI helpers ----

func jstring2go(env *C.JNIEnv, s C.jstring) string {
	var isCopy C.jboolean
	chars := C._go_GetStringUTFChars(env, s, &isCopy)
	if chars == nil {
		return ""
	}
	defer C._go_ReleaseStringUTFChars(env, s, chars)
	return C.GoString(chars)
}

func go2jstring(env *C.JNIEnv, s string) C.jstring {
	return C._go_NewStringUTF(env, C.CString(s))
}

func jresult(env *C.JNIEnv, data interface{}) C.jstring {
	b, _ := json.Marshal(map[string]interface{}{
		"success": true,
		"data":    data,
	})
	return go2jstring(env, string(b))
}

func jerror(env *C.JNIEnv, err error) C.jstring {
	return jerrorStr(env, err.Error())
}

func jerrorStr(env *C.JNIEnv, msg string) C.jstring {
	b, _ := json.Marshal(map[string]interface{}{
		"success": false,
		"error":   msg,
	})
	return go2jstring(env, string(b))
}

// ---- JNI exports ----
// Kotlin class: com.openlist.bridge.OpenListDriver
// JNI names: Java_com_openlist_bridge_OpenListDriver_n{Method}

//export Java_com_openlist_bridge_OpenListDriver_nWarmup
func Java_com_openlist_bridge_OpenListDriver_nWarmup(env *C.JNIEnv, clazz C.jclass) C.jstring {
	// Minimal function to initialize Go runtime (scheduler, GC, etc.)
	// before any complex JNI calls. Call this right after System.loadLibrary.
	return jresult(env, map[string]string{"status": "ok"})
}

//export Java_com_openlist_bridge_OpenListDriver_nCreate
func Java_com_openlist_bridge_OpenListDriver_nCreate(env *C.JNIEnv, clazz C.jclass, driverType, configJSON C.jstring) C.jstring {
	dt := jstring2go(env, driverType)
	cj := jstring2go(env, configJSON)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h := newHandle()
	drv, err := initStorage(ctx, dt, cj)
	if err != nil {
		return jerror(env, err)
	}
	storeInstance(h, &instance{drv: drv, drvName: dt})
	return jresult(env, map[string]string{"handle": h})
}

//export Java_com_openlist_bridge_OpenListDriver_nList
func Java_com_openlist_bridge_OpenListDriver_nList(env *C.JNIEnv, clazz C.jclass, handle, path C.jstring) C.jstring {
	inst, err := getInstance(jstring2go(env, handle))
	if err != nil {
		return jerror(env, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	p := jstring2go(env, path)
	if p == "" {
		p = "/"
	}

	dir := rootObj(inst.drv)
	if p != "/" {
		dir, err = resolveDir(ctx, inst.drv, p)
		if err != nil {
			return jerror(env, err)
		}
	}

	files, err := inst.drv.List(ctx, dir, model.ListArgs{})
	if err != nil {
		return jerror(env, err)
	}
	result := make([]cloudFile, 0, len(files))
	for _, f := range files {
		result = append(result, objToCloudFile(f))
	}
	return jresult(env, result)
}

//export Java_com_openlist_bridge_OpenListDriver_nGet
func Java_com_openlist_bridge_OpenListDriver_nGet(env *C.JNIEnv, clazz C.jclass, handle, path C.jstring) C.jstring {
	inst, err := getInstance(jstring2go(env, handle))
	if err != nil {
		return jerror(env, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	obj, err := getObj(ctx, inst.drv, jstring2go(env, path))
	if err != nil {
		return jerror(env, err)
	}
	return jresult(env, objToCloudFile(obj))
}

//export Java_com_openlist_bridge_OpenListDriver_nGetDownloadUrl
func Java_com_openlist_bridge_OpenListDriver_nGetDownloadUrl(env *C.JNIEnv, clazz C.jclass, handle, path C.jstring) C.jstring {
	inst, err := getInstance(jstring2go(env, handle))
	if err != nil {
		return jerror(env, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	obj, err := getObj(ctx, inst.drv, jstring2go(env, path))
	if err != nil {
		return jerror(env, err)
	}
	if obj.IsDir() {
		return jerrorStr(env, "cannot get download url for a directory")
	}

	link, err := inst.drv.Link(ctx, obj, model.LinkArgs{})
	if err != nil {
		return jerror(env, err)
	}
	return jresult(env, map[string]interface{}{
		"url":     link.URL,
		"expires": link.Expiration,
	})
}

//export Java_com_openlist_bridge_OpenListDriver_nUploadFromFile
func Java_com_openlist_bridge_OpenListDriver_nUploadFromFile(env *C.JNIEnv, clazz C.jclass, handle, parentPath, fileName, localFilePath, mimeType C.jstring) C.jstring {
	inst, err := getInstance(jstring2go(env, handle))
	if err != nil {
		return jerror(env, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	pp := jstring2go(env, parentPath)
	fn := jstring2go(env, fileName)
	lp := jstring2go(env, localFilePath)
	mt := jstring2go(env, mimeType)
	if mt == "" {
		mt = "application/octet-stream"
	}

	parentDir, err := resolveDir(ctx, inst.drv, pp)
	if err != nil {
		return jerror(env, err)
	}

	f, err := os.Open(lp)
	if err != nil {
		return jerror(env, fmt.Errorf("open file: %w", err))
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
			Name: fn,
			Size: fileSize,
		},
		Reader:   io.LimitReader(f, fileSize),
		Mimetype: mt,
	}

	if putResult, ok := inst.drv.(driver.PutResult); ok {
		obj, err := putResult.Put(ctx, parentDir, fileStream, nil)
		if err != nil {
			return jerror(env, err)
		}
		if obj != nil {
			return jresult(env, objToCloudFile(obj))
		}
		return jresult(env, map[string]string{"name": fn})
	}

	if putter, ok := inst.drv.(driver.Put); ok {
		err = putter.Put(ctx, parentDir, fileStream, nil)
		if err != nil {
			return jerror(env, err)
		}
		return jresult(env, map[string]string{"name": fn})
	}

	return jerrorStr(env, "driver does not support upload")
}

//export Java_com_openlist_bridge_OpenListDriver_nMkdir
func Java_com_openlist_bridge_OpenListDriver_nMkdir(env *C.JNIEnv, clazz C.jclass, handle, parentPath, dirName C.jstring) C.jstring {
	inst, err := getInstance(jstring2go(env, handle))
	if err != nil {
		return jerror(env, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	parentDir, err := resolveDir(ctx, inst.drv, jstring2go(env, parentPath))
	if err != nil {
		return jerror(env, err)
	}
	dn := jstring2go(env, dirName)

	if mkdirResult, ok := inst.drv.(driver.MkdirResult); ok {
		obj, err := mkdirResult.MakeDir(ctx, parentDir, dn)
		if err != nil {
			return jerror(env, err)
		}
		return jresult(env, objToCloudFile(obj))
	}

	if mkdir, ok := inst.drv.(driver.Mkdir); ok {
		err = mkdir.MakeDir(ctx, parentDir, dn)
		if err != nil {
			return jerror(env, err)
		}
		return jresult(env, map[string]string{"name": dn})
	}

	return jerrorStr(env, "driver does not support mkdir")
}

//export Java_com_openlist_bridge_OpenListDriver_nDelete
func Java_com_openlist_bridge_OpenListDriver_nDelete(env *C.JNIEnv, clazz C.jclass, handle, path C.jstring) C.jstring {
	inst, err := getInstance(jstring2go(env, handle))
	if err != nil {
		return jerror(env, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	obj, err := getObj(ctx, inst.drv, jstring2go(env, path))
	if err != nil {
		return jerror(env, err)
	}

	remover, ok := inst.drv.(driver.Remove)
	if !ok {
		return jerrorStr(env, "driver does not support delete")
	}
	err = remover.Remove(ctx, obj)
	if err != nil {
		return jerror(env, err)
	}
	return jresult(env, map[string]string{"status": "deleted"})
}

//export Java_com_openlist_bridge_OpenListDriver_nRename
func Java_com_openlist_bridge_OpenListDriver_nRename(env *C.JNIEnv, clazz C.jclass, handle, path, newName C.jstring) C.jstring {
	inst, err := getInstance(jstring2go(env, handle))
	if err != nil {
		return jerror(env, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	obj, err := getObj(ctx, inst.drv, jstring2go(env, path))
	if err != nil {
		return jerror(env, err)
	}
	nn := jstring2go(env, newName)

	if renameResult, ok := inst.drv.(driver.RenameResult); ok {
		newObj, err := renameResult.Rename(ctx, obj, nn)
		if err != nil {
			return jerror(env, err)
		}
		return jresult(env, objToCloudFile(newObj))
	}

	if renamer, ok := inst.drv.(driver.Rename); ok {
		err = renamer.Rename(ctx, obj, nn)
		if err != nil {
			return jerror(env, err)
		}
		return jresult(env, map[string]string{"name": nn})
	}

	return jerrorStr(env, "driver does not support rename")
}

//export Java_com_openlist_bridge_OpenListDriver_nMove
func Java_com_openlist_bridge_OpenListDriver_nMove(env *C.JNIEnv, clazz C.jclass, handle, srcPath, dstDirPath C.jstring) C.jstring {
	inst, err := getInstance(jstring2go(env, handle))
	if err != nil {
		return jerror(env, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srcObj, err := getObj(ctx, inst.drv, jstring2go(env, srcPath))
	if err != nil {
		return jerror(env, err)
	}
	dstDir, err := resolveDir(ctx, inst.drv, jstring2go(env, dstDirPath))
	if err != nil {
		return jerror(env, err)
	}

	if moveResult, ok := inst.drv.(driver.MoveResult); ok {
		newObj, err := moveResult.Move(ctx, srcObj, dstDir)
		if err != nil {
			return jerror(env, err)
		}
		return jresult(env, objToCloudFile(newObj))
	}

	if mover, ok := inst.drv.(driver.Move); ok {
		err = mover.Move(ctx, srcObj, dstDir)
		if err != nil {
			return jerror(env, err)
		}
		return jresult(env, map[string]string{"status": "moved"})
	}

	return jerrorStr(env, "driver does not support move")
}

//export Java_com_openlist_bridge_OpenListDriver_nCopy
func Java_com_openlist_bridge_OpenListDriver_nCopy(env *C.JNIEnv, clazz C.jclass, handle, srcPath, dstDirPath C.jstring) C.jstring {
	inst, err := getInstance(jstring2go(env, handle))
	if err != nil {
		return jerror(env, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srcObj, err := getObj(ctx, inst.drv, jstring2go(env, srcPath))
	if err != nil {
		return jerror(env, err)
	}
	dstDir, err := resolveDir(ctx, inst.drv, jstring2go(env, dstDirPath))
	if err != nil {
		return jerror(env, err)
	}

	if copyResult, ok := inst.drv.(driver.CopyResult); ok {
		newObj, err := copyResult.Copy(ctx, srcObj, dstDir)
		if err != nil {
			return jerror(env, err)
		}
		return jresult(env, objToCloudFile(newObj))
	}

	if copier, ok := inst.drv.(driver.Copy); ok {
		err = copier.Copy(ctx, srcObj, dstDir)
		if err != nil {
			return jerror(env, err)
		}
		return jresult(env, map[string]string{"status": "copied"})
	}

	return jerrorStr(env, "driver does not support copy")
}

//export Java_com_openlist_bridge_OpenListDriver_nGetStorageDetails
func Java_com_openlist_bridge_OpenListDriver_nGetStorageDetails(env *C.JNIEnv, clazz C.jclass, handle C.jstring) C.jstring {
	inst, err := getInstance(jstring2go(env, handle))
	if err != nil {
		return jerror(env, err)
	}

	wd, ok := inst.drv.(driver.WithDetails)
	if !ok {
		return jerrorStr(env, "driver does not support storage details")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	details, err := wd.GetDetails(ctx)
	if err != nil {
		return jerror(env, err)
	}
	return jresult(env, map[string]int64{
		"total_space": details.TotalSpace,
		"used_space":  details.UsedSpace,
	})
}

//export Java_com_openlist_bridge_OpenListDriver_nDestroy
func Java_com_openlist_bridge_OpenListDriver_nDestroy(env *C.JNIEnv, clazz C.jclass, handle C.jstring) {
	h := jstring2go(env, handle)
	inst, err := removeInstance(h)
	if err != nil {
		return
	}
	_ = inst.drv.Drop(context.Background())
}

// ---- internal helpers ----

func initStorage(ctx context.Context, driverName, additionJSON string) (driver.Driver, error) {
	// Initialize minimal config for drivers/base HTTP client
	if conf.Conf == nil {
		conf.Conf = &conf.Config{TlsInsecureSkipVerify: false}
	}
	base.InitClient()
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

func newHandle() string {
	return uuid.New().String()
}

func storeInstance(h string, inst *instance) {
	instMu.Lock()
	insts[h] = inst
	instMu.Unlock()
}

func getInstance(h string) (*instance, error) {
	instMu.RLock()
	inst, ok := insts[h]
	instMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("invalid handle: %s", h)
	}
	return inst, nil
}

func removeInstance(h string) (*instance, error) {
	instMu.Lock()
	inst, ok := insts[h]
	if ok {
		delete(insts, h)
	}
	instMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("invalid handle: %s", h)
	}
	return inst, nil
}

func rootObj(d driver.Driver) model.Obj {
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

// ---- data types ----

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

func main() {}
