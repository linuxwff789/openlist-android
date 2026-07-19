# OpenList Bridge — Android JNI library

OpenList 驱动移植 Android 版。通过 Go → JNI 桥接，提供 WebDAV / 天翼云189 / 阿里云盘 Open 的基础上传下载能力。

## 结构

```
bridge/
├── bridge.go              Go JNI 桥 (12个导出函数)
├── OpenListBridge.kt      Kotlin 协程 API (实现 CloudDriver 接口)
├── build_android.sh       本地编译脚本 (需 NDK)
├── setup-repo.sh          一键建仓推送
├── cmd/debug/main.go      Termux 调试 CLI
```

## 构建产物

| 文件 | 用途 |
|------|------|
| `libopenlist.so` | ARM64 动态库，放 `jniLibs/arm64-v8a/` |
| `openlist-cli.arm64` | Termux 调试用 CLI |
| `OpenListBridge.kt` | Kotlin 包装类 |

## 在 Termux 调试

```bash
# 从 Release 下载 openlist-cli.arm64
chmod +x openlist-cli.arm64

# WebDAV
./openlist-cli.arm64 webdav '{"address":"https://webdav.me","username":"u","password":"p"}' list /

# 阿里云盘
./openlist-cli.arm64 aliyundrive_open '{"refresh_token":"xxx"}' list /Photos

# 天翼云
./openlist-cli.arm64 189Cloud '{"username":"189xxx","password":"xxx"}' info
```

## 集成到 Android 项目

```kotlin
// 1. 复制 OpenListBridge.kt 到项目
// 2. build.gradle.kts 加依赖:
//    implementation("org.jetbrains.kotlinx:kotlinx-serialization-json:1.7.x")
// 3. jniLibs/arm64-v8a/libopenlist.so 放好

val driver = OpenListDriver.create("WebDav", """
  {"address":"https://webdav.me","username":"u","password":"p"}
""")
val files = driver.list("/docs")
driver.destroy()
```

## 支持的驱动

| Driver | 类型 | 认证方式 |
|--------|------|---------|
| WebDav | 标准协议 | Basic Auth |
| 189Cloud | 国内网盘 | RSA加密密码 + Cookie |
| AliyundriveOpen | 国内网盘 | OAuth2 Refresh Token |
| *(接口已预留)* | — | 实现 CloudDriver 即可扩展 |

## 构建

```bash
# GitHub Actions (推荐): 推送后自动编译
# 本地编译:
ANDROID_NDK_HOME=$HOME/Android/ndk/27.0.12077973 bash bridge/build_android.sh
```
