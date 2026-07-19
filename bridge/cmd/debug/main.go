// Debug CLI for OpenList bridge — test drivers without Android/JNI.
//
// Usage:
//   go run ./bridge/cmd/debug webdav '{"address":"https://...","username":"u","password":"p"}' list /
//   go run ./bridge/cmd/debug webdav '{"address":"...","username":"u","password":"p"}' upload / /tmp/test.txt
//   go run ./bridge/cmd/debug aliyundrive_open '{"refresh_token":"..."}' list /
//
// Build:
//   go build -o openlist-cli ./bridge/cmd/debug
//
// Cross-compile for Termux (arm64):
//   GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build -o openlist-cli.arm64 ./bridge/cmd/debug

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Direct driver imports — no op.GetDriver, which breaks argv on Android
	"github.com/OpenListTeam/OpenList/v4/drivers/189"
	"github.com/OpenListTeam/OpenList/v4/drivers/189pc"
	"github.com/OpenListTeam/OpenList/v4/drivers/aliyundrive_open"
	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/drivers/webdav"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"

	"gorm.io/gorm"
	"github.com/glebarez/sqlite"
)

func main() {
	if len(os.Args) < 2 {
		showHelp()
	}

	// HACK: On some Android Go builds, os.Args[1] == os.Args[0] (binary path).
	// Scan for first non-path argument that looks like a driver name.
	driverName := ""
	for _, a := range os.Args[1:] {
		if !strings.HasPrefix(a, "/") && !strings.HasPrefix(a, ".") {
			driverName = a
			break
		}
	}
	if driverName == "" {
		fatalf("cannot find driver name in args: %v", os.Args)
	}
	// Rebuild remaining args after driver name
	remaining := []string{}
	found := false
	for _, a := range os.Args[1:] {
		if a == driverName && !found {
			found = true
			continue
		}
		if found {
			remaining = append(remaining, a)
		}
	}
	if len(remaining) < 2 {
		showHelp()
	}
	configJSON := remaining[0]
	cmd := remaining[1]
	args := remaining[2:]

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Initialize minimal config — needed by drivers/base for HTTP client
	conf.Conf = &conf.Config{
		TlsInsecureSkipVerify: false,
	}
	base.InitClient()
	// Initialize in-memory SQLite for GORM (needed by op.MustSaveDriverStorage)
	sqlDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		fatalf("failed to open db: %v", err)
	}
	db.Init(sqlDB)
	// Override DNS to avoid Android TUN VPN DNS breakage
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "udp", "8.8.8.8:53")
		},
	}

	var drv driver.Driver
	storage := model.Storage{
		MountPath: "/",
		Driver:    driverName,
		Addition:  configJSON,
		Modified:  time.Now(),
	}

	switch driverName {
	case "WebDav":
		d := &webdav.WebDav{}
		drv = d
	case "189Cloud":
		d := &_189.Cloud189{}
		drv = d
	case "189CloudPC":
		d := &_189pc.Cloud189PC{}
		drv = d
	case "AliyundriveOpen":
		d := &aliyundrive_open.AliyundriveOpen{}
		drv = d
	default:
		fatalf("unknown driver: %s (supported: WebDav, 189Cloud, AliyundriveOpen)", driverName)
	}

	drv.SetStorage(storage)
	if err := utils.Json.UnmarshalFromString(configJSON, drv.GetAddition()); err != nil {
		fatalf("bad config: %v", err)
	}
	if err := drv.Init(ctx); err != nil {
		fatalf("init: %v", err)
	}
	defer drv.Drop(ctx)

	switch cmd {
	case "list":
		runList(ctx, drv, args)
	case "get":
		runGet(ctx, drv, args)
	case "url":
		runURL(ctx, drv, args)
	case "mkdir":
		runMkdir(ctx, drv, args)
	case "delete":
		runDelete(ctx, drv, args)
	case "rename":
		runRename(ctx, drv, args)
	case "move":
		runMove(ctx, drv, args)
	case "copy":
		runCopy(ctx, drv, args)
	case "upload":
		runUpload(ctx, drv, args)
	case "info":
		runInfo(ctx, drv)
	default:
		fatalf("unknown command: %s", cmd)
	}
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

func resolve(ctx context.Context, d driver.Driver, path string) (model.Obj, error) {
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

func walkTo(ctx context.Context, d driver.Driver, path string) (model.Obj, error) {
	if path == "" || path == "/" {
		return rootObj(d), nil
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return walkParts(ctx, d, rootObj(d), parts)
}

func walkParts(ctx context.Context, d driver.Driver, dir model.Obj, parts []string) (model.Obj, error) {
	if len(parts) == 0 {
		return dir, nil
	}
	files, err := d.List(ctx, dir, model.ListArgs{})
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	for _, f := range files {
		if f.GetName() == parts[0] {
			if len(parts) == 1 {
				return f, nil
			}
			if !f.IsDir() {
				return nil, fmt.Errorf("not a dir: %s", parts[0])
			}
			return walkParts(ctx, d, f, parts[1:])
		}
	}
	return nil, fmt.Errorf("not found: %s", parts[0])
}

func printJSON(v interface{}) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func runList(ctx context.Context, d driver.Driver, args []string) {
	p := "/"
	if len(args) > 0 {
		p = args[0]
	}
	dir, err := resolve(ctx, d, p)
	if err != nil {
		fatalf("resolve: %v", err)
	}
	files, err := d.List(ctx, dir, model.ListArgs{})
	if err != nil {
		fatalf("list: %v", err)
	}
	fmt.Printf("  %-40s %-10s %-5s  %s\n", "Name", "Size", "Dir", "Modified")
	fmt.Println(strings.Repeat("-", 80))
	for _, f := range files {
		icon := "📄"
		if f.IsDir() {
			icon = "📁"
		}
		t := f.ModTime().Format("2006-01-02 15:04")
		fmt.Printf("  %-40s %-10d %-5s  %s\n", f.GetName(), f.GetSize(), icon, t)
	}
}

func runGet(ctx context.Context, d driver.Driver, args []string) {
	if len(args) < 1 {
		fatalf("need path")
	}
	obj, err := resolve(ctx, d, args[0])
	if err != nil {
		fatalf("get: %v", err)
	}
	printJSON(obj)
}

func runURL(ctx context.Context, d driver.Driver, args []string) {
	if len(args) < 1 {
		fatalf("need path")
	}
	obj, err := resolve(ctx, d, args[0])
	if err != nil {
		fatalf("get: %v", err)
	}
	if obj.IsDir() {
		fatalf("cannot get url for directory")
	}
	link, err := d.Link(ctx, obj, model.LinkArgs{})
	if err != nil {
		fatalf("link: %v", err)
	}
	printJSON(link)
}

func runMkdir(ctx context.Context, d driver.Driver, args []string) {
	if len(args) < 2 {
		fatalf("need <parent> <name>")
	}
	parent, err := resolve(ctx, d, args[0])
	if err != nil {
		fatalf("resolve parent: %v", err)
	}
	if m, ok := d.(driver.Mkdir); ok {
		err = m.MakeDir(ctx, parent, args[1])
		if err != nil {
			fatalf("mkdir: %v", err)
		}
		fmt.Println("ok")
		return
	}
	if mr, ok := d.(driver.MkdirResult); ok {
		obj, err := mr.MakeDir(ctx, parent, args[1])
		if err != nil {
			fatalf("mkdir: %v", err)
		}
		printJSON(obj)
		return
	}
	fatalf("driver does not support mkdir")
}

func runDelete(ctx context.Context, d driver.Driver, args []string) {
	if len(args) < 1 {
		fatalf("need path")
	}
	obj, err := resolve(ctx, d, args[0])
	if err != nil {
		fatalf("get: %v", err)
	}
	remover, ok := d.(driver.Remove)
	if !ok {
		fatalf("driver does not support delete")
	}
	if err := remover.Remove(ctx, obj); err != nil {
		fatalf("delete: %v", err)
	}
	fmt.Println("deleted")
}

func runRename(ctx context.Context, d driver.Driver, args []string) {
	if len(args) < 2 {
		fatalf("need <path> <new-name>")
	}
	obj, err := resolve(ctx, d, args[0])
	if err != nil {
		fatalf("get: %v", err)
	}
	if r, ok := d.(driver.Rename); ok {
		if err := r.Rename(ctx, obj, args[1]); err != nil {
			fatalf("rename: %v", err)
		}
		fmt.Println("ok")
		return
	}
	if rr, ok := d.(driver.RenameResult); ok {
		o, err := rr.Rename(ctx, obj, args[1])
		if err != nil {
			fatalf("rename: %v", err)
		}
		printJSON(o)
		return
	}
	fatalf("driver does not support rename")
}

func runMove(ctx context.Context, d driver.Driver, args []string) {
	if len(args) < 2 {
		fatalf("need <src> <dst-dir>")
	}
	src, err := resolve(ctx, d, args[0])
	if err != nil {
		fatalf("get src: %v", err)
	}
	dst, err := resolve(ctx, d, args[1])
	if err != nil {
		fatalf("resolve dst: %v", err)
	}
	if m, ok := d.(driver.Move); ok {
		if err := m.Move(ctx, src, dst); err != nil {
			fatalf("move: %v", err)
		}
		fmt.Println("ok")
		return
	}
	if mr, ok := d.(driver.MoveResult); ok {
		o, err := mr.Move(ctx, src, dst)
		if err != nil {
			fatalf("move: %v", err)
		}
		printJSON(o)
		return
	}
	fatalf("driver does not support move")
}

func runCopy(ctx context.Context, d driver.Driver, args []string) {
	if len(args) < 2 {
		fatalf("need <src> <dst-dir>")
	}
	src, err := resolve(ctx, d, args[0])
	if err != nil {
		fatalf("get src: %v", err)
	}
	dst, err := resolve(ctx, d, args[1])
	if err != nil {
		fatalf("resolve dst: %v", err)
	}
	if c, ok := d.(driver.Copy); ok {
		if err := c.Copy(ctx, src, dst); err != nil {
			fatalf("copy: %v", err)
		}
		fmt.Println("ok")
		return
	}
	if cr, ok := d.(driver.CopyResult); ok {
		o, err := cr.Copy(ctx, src, dst)
		if err != nil {
			fatalf("copy: %v", err)
		}
		printJSON(o)
		return
	}
	fatalf("driver does not support copy")
}

func runUpload(ctx context.Context, d driver.Driver, args []string) {
	if len(args) < 2 {
		fatalf("need <parent> <local-file>")
	}
	parent, err := resolve(ctx, d, args[0])
	if err != nil {
		fatalf("resolve parent: %v", err)
	}
	localPath := args[1]
	f, err := os.Open(localPath)
	if err != nil {
		fatalf("open file: %v", err)
	}
	defer f.Close()
	stat, _ := f.Stat()
	name := filepath.Base(localPath)

	fs := &stream.FileStream{
		Ctx: ctx,
		Obj: &model.Object{
			Name: name,
			Size: stat.Size(),
		},
		Reader:   io.LimitReader(f, stat.Size()),
		Mimetype: "application/octet-stream",
	}

	if pr, ok := d.(driver.PutResult); ok {
		obj, err := pr.Put(ctx, parent, fs, nil)
		if err != nil {
			fatalf("upload: %v", err)
		}
		printJSON(obj)
		return
	}
	if p, ok := d.(driver.Put); ok {
		if err := p.Put(ctx, parent, fs, nil); err != nil {
			fatalf("upload: %v", err)
		}
		fmt.Println("uploaded")
		return
	}
	fatalf("driver does not support upload")
}

func runInfo(ctx context.Context, d driver.Driver) {
	wd, ok := d.(driver.WithDetails)
	if !ok {
		fatalf("driver does not support storage details")
	}
	details, err := wd.GetDetails(ctx)
	if err != nil {
		fatalf("details: %v", err)
	}
	printJSON(details)
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(1)
}

func showHelp() {
	fmt.Fprintf(os.Stderr, `OpenList debug CLI — test drivers from terminal.

Usage:
  %s <driver> <config-json> <command> [args...]

Drivers: webdav, 189Cloud, 189CloudPC, aliyundrive_open

Commands:
  list <path>                    List directory
  get <path>                     Get file info
  url <path>                     Get download URL
  mkdir <parent> <name>          Create directory
  delete <path>                  Delete file/dir
  rename <path> <new-name>       Rename
  move <src> <dst-dir>           Move
  copy <src> <dst-dir>           Copy
  upload <parent> <local-file>   Upload file
  info                           Storage details

Examples:
  %s webdav '{"address":"https://webdav.me","username":"u","password":"p"}' list /
  %s aliyundrive_open '{"refresh_token":"xxx"}' list /Photos
`,
		os.Args[0], os.Args[0], os.Args[0])
	os.Exit(1)
}
