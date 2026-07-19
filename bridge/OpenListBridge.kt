package com.openlist.bridge

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.decodeFromJsonElement
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive

/**
 * OpenList JNI bridge — Android native library wrapper.
 *
 * Loads libopenlist.so (built from OpenList/bridge/) and provides
 * typed Kotlin coroutine APIs for cloud drive operations.
 *
 * Usage:
 *   val driver = OpenListDriver.create("WebDav", """
 *     {"address":"https://...","username":"u","password":"p"}
 *   """)
 *   driver.list("/")       // ListCloudFile
 *   driver.download("/a.txt") // DownloadInfo
 *   driver.upload("/", UploadFile(...))  // CloudFile
 *   driver.destroy()
 */
class OpenListDriver private constructor(
    private val handle: String
) : CloudDriver {

    companion object {
        private val json = Json { ignoreUnknownKeys = true; isLenient = true }

        /** Load native library */
        init {
            System.loadLibrary("openlist")
        }

        /**
         * Create a new driver instance.
         * @param driverType one of "WebDav", "189Cloud", "AliyundriveOpen"
         * @param configJson driver-specific config JSON (see README)
         * @return OpenListDriver instance
         */
        suspend fun create(driverType: String, configJson: String): OpenListDriver {
            val result = nCreate(driverType, configJson)
            val parsed = json.parseToJsonElement(result).jsonObject
            checkSuccess(parsed, "create driver")
            val handle = parsed["data"]!!.jsonObject["handle"]!!.jsonPrimitive.content
            return OpenListDriver(handle)
        }
    }

    // ---- public API ----

    override suspend fun list(path: String): List<CloudFile> {
        val result = nList(handle, path)
        val parsed = json.parseToJsonElement(result).jsonObject
        checkSuccess(parsed, "list $path")
        val arr = parsed["data"]!!.jsonArray
        return arr.map { json.decodeFromJsonElement<CloudFile>(it) }
    }

    override suspend fun get(path: String): CloudFile? {
        val result = nGet(handle, path)
        val parsed = json.parseToJsonElement(result).jsonObject
        checkSuccess(parsed, "get $path")
        return json.decodeFromJsonElement(parsed["data"]!!)
    }

    override suspend fun getDownloadUrl(path: String): DownloadInfo {
        val result = nGetDownloadUrl(handle, path)
        val parsed = json.parseToJsonElement(result).jsonObject
        checkSuccess(parsed, "get download url $path")
        val data = parsed["data"]!!.jsonObject
        return DownloadInfo(
            url = data["url"]!!.jsonPrimitive.content,
        )
    }

    override suspend fun upload(
        parentPath: String,
        fileName: String,
        localFilePath: String,
        mimeType: String
    ): CloudFile {
        val result = nUploadFromFile(handle, parentPath, fileName, localFilePath, mimeType)
        val parsed = json.parseToJsonElement(result).jsonObject
        checkSuccess(parsed, "upload $fileName")
        return json.decodeFromJsonElement(parsed["data"]!!)
    }

    override suspend fun mkdir(parentPath: String, dirName: String): CloudFile {
        val result = nMkdir(handle, parentPath, dirName)
        val parsed = json.parseToJsonElement(result).jsonObject
        checkSuccess(parsed, "mkdir $dirName")
        return json.decodeFromJsonElement(parsed["data"]!!)
    }

    override suspend fun delete(path: String) {
        val result = nDelete(handle, path)
        val parsed = json.parseToJsonElement(result).jsonObject
        checkSuccess(parsed, "delete $path")
    }

    override suspend fun rename(path: String, newName: String): CloudFile {
        val result = nRename(handle, path, newName)
        val parsed = json.parseToJsonElement(result).jsonObject
        checkSuccess(parsed, "rename $path -> $newName")
        return json.decodeFromJsonElement(parsed["data"]!!)
    }

    override suspend fun move(srcPath: String, dstDirPath: String): CloudFile {
        val result = nMove(handle, srcPath, dstDirPath)
        val parsed = json.parseToJsonElement(result).jsonObject
        checkSuccess(parsed, "move $srcPath -> $dstDirPath")
        return json.decodeFromJsonElement(parsed["data"]!!)
    }

    override suspend fun copy(srcPath: String, dstDirPath: String): CloudFile {
        val result = nCopy(handle, srcPath, dstDirPath)
        val parsed = json.parseToJsonElement(result).jsonObject
        checkSuccess(parsed, "copy $srcPath -> $dstDirPath")
        return json.decodeFromJsonElement(parsed["data"]!!)
    }

    override suspend fun getStorageDetails(): StorageDetails {
        val result = nGetStorageDetails(handle)
        val parsed = json.parseToJsonElement(result).jsonObject
        checkSuccess(parsed, "get storage details")
        return json.decodeFromJsonElement(parsed["data"]!!)
    }

    override fun destroy() {
        nDestroy(handle)
    }

    // ---- JNI native methods ----

    private external fun nCreate(driverType: String, configJson: String): String
    private external fun nList(handle: String, path: String): String
    private external fun nGet(handle: String, path: String): String
    private external fun nGetDownloadUrl(handle: String, path: String): String
    private external fun nUploadFromFile(
        handle: String, parentPath: String, fileName: String,
        localFilePath: String, mimeType: String
    ): String
    private external fun nMkdir(handle: String, parentPath: String, dirName: String): String
    private external fun nDelete(handle: String, path: String): String
    private external fun nRename(handle: String, path: String, newName: String): String
    private external fun nMove(handle: String, srcPath: String, dstDirPath: String): String
    private external fun nCopy(handle: String, srcPath: String, dstDirPath: String): String
    private external fun nGetStorageDetails(handle: String): String
    private external fun nDestroy(handle: String)

    // ---- helpers ----

    private fun checkSuccess(parsed: JsonObject, op: String) {
        if (!parsed["success"]!!.jsonPrimitive.boolean) {
            val err = parsed["error"]?.jsonPrimitive?.content ?: "unknown error"
            throw CloudDriverException("$op failed: $err")
        }
    }
}

// ---- data classes ----

@Serializable
data class CloudFile(
    val id: String,
    val name: String,
    val size: Long = 0,
    @SerialName("is_dir") val isDir: Boolean = false,
    @SerialName("modified_at") val modifiedAt: Long = 0,
    val path: String = "",
    val thumbnail: String? = null
)

@Serializable
data class DownloadInfo(
    val url: String
)

@Serializable
data class StorageDetails(
    @SerialName("total_space") val totalSpace: Long = 0,
    @SerialName("used_space") val usedSpace: Long = 0
)

class CloudDriverException(message: String) : Exception(message)

// ---- interface for future expansion ----

interface CloudDriver {
    suspend fun list(path: String): List<CloudFile>
    suspend fun get(path: String): CloudFile?
    suspend fun getDownloadUrl(path: String): DownloadInfo
    suspend fun upload(parentPath: String, fileName: String, localFilePath: String, mimeType: String): CloudFile
    suspend fun mkdir(parentPath: String, dirName: String): CloudFile
    suspend fun delete(path: String)
    suspend fun rename(path: String, newName: String): CloudFile
    suspend fun move(srcPath: String, dstDirPath: String): CloudFile
    suspend fun copy(srcPath: String, dstDirPath: String): CloudFile
    suspend fun getStorageDetails(): StorageDetails
    fun destroy()
}
