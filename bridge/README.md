# OpenList Bridge — Android JNI Library

阿里云盘 / 天翼云 / WebDAV 的 Android JNI 桥接库。Go → JNI → Kotlin，提供统一的上传下载接口。

## 快速开始

```kotlin
// 1. 复制 OpenListBridge.kt 到项目
// 2. jniLibs/arm64-v8a/libopenlist.so 放好
// 3. build.gradle.kts 加:
//    implementation("org.jetbrains.kotlinx:kotlinx-serialization-json:1.7.x")

val driver = OpenListDriver.create("AliyundriveOpen", """
{
  "refresh_token": "你的refresh_token",
  "use_online_api": true,
  "api_url_address": "https://api.oplist.org/alicloud/renewapi",
  "drive_type": "resource",
  "root_folder_id": "root",
  "remove_way": "trash"
}
""")

// 文件操作
val files = driver.list("/文档")
driver.upload("/文档", "photo.jpg", "/sdcard/DCIM/photo.jpg", "image/jpeg")
val info = driver.getDownloadUrl("/文档/photo.jpg")
driver.mkdir("/文档/新目录")
driver.rename("/文档/photo.jpg", "renamed.jpg")
driver.delete("/文档/renamed.jpg")
driver.destroy()
```

## 支持的驱动

| Driver | 类型 | 状态 | 认证 |
|--------|------|------|------|
| `AliyundriveOpen` | 阿里云盘 | ✅ 完整可用 | OAuth2 Refresh Token |
| `WebDav` | 标准协议 | ✅ 可用 | Basic Auth |
| `189Cloud` | 天翼云 | ❌ 设备校验受阻 | RSA + Cookie |
| `189CloudPC` | 天翼云PC | ⚠️ 需扫码 | 密码/二维码 |

## 核心接口

```kotlin
interface CloudDriver {
    suspend fun list(path: String): List<CloudFile>
    suspend fun get(path: String): CloudFile?
    suspend fun getDownloadUrl(path: String): DownloadInfo
    suspend fun upload(parentPath: String, fileName: String,
                       localFilePath: String, mimeType: String): CloudFile
    suspend fun mkdir(parentPath: String, dirName: String): CloudFile
    suspend fun delete(path: String)
    suspend fun rename(path: String, newName: String): CloudFile
    suspend fun move(srcPath: String, dstDirPath: String): CloudFile
    suspend fun copy(srcPath: String, dstDirPath: String): CloudFile
    suspend fun getStorageDetails(): StorageDetails
    fun destroy()
}
```

## 性能 (阿里云盘 200MB 上传测试)

| 指标 | 数值 |
|------|------|
| 内存占用 (RSS) | ≈4 MB |
| CPU 占用 | ≈0% (IO 密集型) |
| 上传速度 | ≈2 MB/s (受 TUN VPN 限制) |
| 分片上传 | 自动处理 |
| 稳定性 | 全程无崩溃 |

## Termux 调试

从 Release 下载 `openlist-cli.arm64`：

```bash
chmod +x openlist-cli.arm64
# 阿里云盘
./openlist-cli.arm64 AliyundriveOpen '{"refresh_token":"...","use_online_api":true,"api_url_address":"https://api.oplist.org/alicloud/renewapi","drive_type":"resource","root_folder_id":"root","remove_way":"trash"}' list /

# WebDAV
./openlist-cli.arm64 WebDav '{"address":"https://webdav.me","username":"u","password":"p"}' list /
```

## 项目结构

```
bridge/
├── bridge.go              Go JNI 桥 (12个JNI导出函数)
├── OpenListBridge.kt      Kotlin 协程 API (CloudDriver 接口)
├── build_android.sh       本地编译脚本 (需 NDK r27+)
├── cmd/debug/main.go      Termux 调试 CLI
└── README.md              本文件
```

## 构建

```bash
# GitHub Actions (推荐): 推送后自动编译
# 或本地编译:
ANDROID_NDK_HOME=$HOME/Android/ndk/27.0.12077973 bash bridge/build_android.sh
```
