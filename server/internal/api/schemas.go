package api

import (
	"time"

	"github.com/Sakana-1314/MList/server/internal/cache"
	"github.com/Sakana-1314/MList/server/internal/media"
	"github.com/Sakana-1314/MList/server/internal/search"
	"github.com/Sakana-1314/MList/server/internal/store"
	"github.com/Sakana-1314/MList/server/internal/vfs"
)

// This file holds the request and response bodies that the OpenAPI document is
// generated from. Every type and field carries a Chinese comment: swag copies
// those comments into the spec, so they are the single source of truth for the
// API documentation. Moving an inline struct or gin.H into a named type here is
// what makes its fields appear (with descriptions) in the spec.

// ListResponse 目录列表。
//
// @Description 目录列表
type ListResponse struct {
	Path     string      `json:"path" extensions:"x-apifox-mock=/"`        // 规范化后的目录虚拟路径
	Writable bool        `json:"writable" extensions:"x-apifox-mock=true"` // 当前目录是否可写
	Entries  []vfs.Entry `json:"entries"`                                  // 目录下的文件与子目录
}

// TextResponse 文本文件内容。
//
// @Description 文本文件内容
type TextResponse struct {
	Content   string `json:"content"`   // 文件文本内容（按 UTF-8 解码）
	Truncated bool   `json:"truncated"` // 是否因超过 4 MiB 而被截断
}

// BatchFailure 批量操作中失败的单个条目。
//
// @Description 批量操作中失败的单个条目
type BatchFailure struct {
	Path  string `json:"path"`  // 失败的虚拟路径
	Error string `json:"error"` // 失败原因
}

// BatchResult 批量操作结果（移动、复制、删除）。
//
// @Description 批量操作结果，含成功的路径与失败的条目
type BatchResult struct {
	Succeeded []string       `json:"succeeded"` // 操作成功的虚拟路径列表
	Failed    []BatchFailure `json:"failed"`    // 操作失败的条目及原因
}

// LoginRequest 登录请求。
//
// @Description 登录请求体
type LoginRequest struct {
	Username string `json:"username" binding:"required"` // 用户名
	Password string `json:"password" binding:"required"` // 登录密码
}

// LoginResponse 登录成功后的会话信息。
//
// @Description 登录成功后的会话信息
type LoginResponse struct {
	Token     string     `json:"token"`      // 会话令牌
	ExpiresAt time.Time  `json:"expires_at"` // 会话过期时间
	User      store.User `json:"user"`       // 登录用户信息
}

// PasswordRequest 修改自己的密码。
//
// @Description 修改自己密码的请求体
type PasswordRequest struct {
	OldPassword string `json:"old_password" binding:"required"` // 当前密码
	NewPassword string `json:"new_password" binding:"required"` // 新密码，至少 6 位
}

// PasswordResponse 改密后签发的新会话。
//
// @Description 修改密码后签发的新会话
type PasswordResponse struct {
	Token     string    `json:"token"`      // 新的会话令牌
	ExpiresAt time.Time `json:"expires_at"` // 新会话的过期时间
}

// MkdirRequest 新建目录。
//
// @Description 新建目录的请求体
type MkdirRequest struct {
	Path string `json:"path" binding:"required"` // 新目录的完整虚拟路径
}

// RenameRequest 重命名。
//
// @Description 重命名的请求体
type RenameRequest struct {
	Path string `json:"path" binding:"required"` // 要重命名的文件或目录的虚拟路径
	Name string `json:"name" binding:"required"` // 新名称（不含路径）
}

// DeleteRequest 删除。
//
// @Description 删除的请求体
type DeleteRequest struct {
	Paths []string `json:"paths" binding:"required"` // 要删除的虚拟路径列表，目录会被递归删除
}

// TransferRequest 移动 / 复制的请求体。
//
// @Description 移动或复制的请求体
type TransferRequest struct {
	Sources   []string `json:"sources" binding:"required"`  // 源文件或目录的虚拟路径列表
	DestDir   string   `json:"dest_dir" binding:"required"` // 目标目录的虚拟路径
	Overwrite bool     `json:"overwrite"`                   // 目标已存在时是否覆盖
}

// SearchResult 单条搜索结果。
//
// @Description 单条搜索结果
type SearchResult struct {
	vfs.Entry
	Score   int   `json:"score"`   // 匹配得分，越高越相关
	Matches []int `json:"matches"` // 名称中命中的字符下标（按 Unicode 字符计）
}

// SearchResponse 搜索结果。
//
// @Description 搜索结果
type SearchResponse struct {
	Results  []SearchResult `json:"results"`  // 命中的条目，按相关度从高到低
	Total    int            `json:"total"`    // 命中总数（可能大于返回数量）
	Indexing bool           `json:"indexing"` // 索引是否正在构建（此时结果可能不完整）
	TookMS   float64        `json:"took_ms"`  // 本次搜索耗时（毫秒）
}

// TranscodeRequest 创建 HLS 转码会话。
//
// @Description 创建 HLS 转码会话的请求体
type TranscodeRequest struct {
	Path       string `json:"path" binding:"required"`                  // 视频文件的虚拟路径
	Quality    string `json:"quality" enums:"original,1080p,720p,480p"` // 输出画质
	Bitrate    int    `json:"bitrate"`                                  // 视频码率（kbps），200 至 80000；0 或省略表示按画质自动选择
	Codec      string `json:"codec" enums:"h264,h265"`                  // 输出视频编码；省略时服务器支持 HEVC 则用 h265，否则 h264。可用值见 MediaInfoResponse.transcode_codecs
	AudioTrack int    `json:"audio_track"`                              // 音轨序号（MediaInfo.audio 下标），0 为第 1 条
}

// TranscodeResponse 转码会话信息。
//
// @Description 转码会话信息
type TranscodeResponse struct {
	ID       string  `json:"id"`       // 转码会话 ID，用于请求播放列表与分片
	Playlist string  `json:"playlist"` // 播放列表路径，如 /api/v1/media/hls/{id}/index.m3u8
	Duration float64 `json:"duration"` // 视频总时长（秒）
	Quality  string  `json:"quality"`  // 实际生效的画质
	Bitrate  int     `json:"bitrate"`  // 请求的视频码率（kbps），0 表示按画质自动选择
	Codec    string  `json:"codec"`    // 输出视频编码
}

// MediaInfoResponse 媒体信息及服务器可用的转码编码。
//
// @Description 媒体信息及服务器可用的转码编码
type MediaInfoResponse struct {
	media.MediaInfo
	TranscodeCodecs []string `json:"transcode_codecs"` // 服务器可以转码输出的视频编码（h264、h265），取决于 ffmpeg 的编码器
}

// CreateTokenRequest 创建访问令牌。
//
// @Description 创建访问令牌的请求体
type CreateTokenRequest struct {
	Name          string   `json:"name" binding:"required"`   // 令牌名称，便于辨认，最长 128 字符
	Scopes        []string `json:"scopes" enums:"read,write"` // 权限范围：read 只读，write 可写；至少包含 read
	ExpiresInDays int      `json:"expires_in_days"`           // 有效期天数；0 或省略表示永不过期，最大 3650
}

// TokenWithSecret 创建访问令牌的响应，含一次性展示的明文。
//
// @Description 创建访问令牌的响应，含一次性的令牌明文
type TokenWithSecret struct {
	store.AccessToken
	Token string `json:"token"` // 令牌明文（ml_ 前缀），可随时通过 /tokens/{id}/secret 再次读取
}

// TokenSecret 访问令牌明文。
//
// @Description 访问令牌明文
type TokenSecret struct {
	Token string `json:"token"` // 令牌明文
}

// CreateS3KeyRequest 创建 S3 密钥。
//
// @Description 创建 S3 密钥的请求体
type CreateS3KeyRequest struct {
	Name     string `json:"name"`      // 备注名称，便于辨认，最长 128 字符
	ReadOnly bool   `json:"read_only"` // 是否只读；true 时该密钥只能读取
}

// S3KeyWithSecret 创建 S3 密钥的响应，含 Secret。
//
// @Description 创建 S3 密钥的响应，含 Secret
type S3KeyWithSecret struct {
	store.S3Key
	SecretKey string `json:"secret_key"` // S3 Secret Key（访问密钥对应的私钥）
}

// S3KeySecret S3 密钥的 Secret。
//
// @Description S3 密钥的 Secret
type S3KeySecret struct {
	SecretKey string `json:"secret_key"` // S3 Secret Key（访问密钥对应的私钥）
}

// UserPassword 用户的当前密码。
//
// @Description 用户的当前密码
type UserPassword struct {
	Password string `json:"password"` // 当前密码明文；known 为 false 时为空串
	Known    bool   `json:"known"`    // 是否还能查到密码；升级前创建的用户为 false，此时只能重置
}

// UserInput 创建 / 更新用户的请求体。
//
// @Description 创建或更新用户的请求体
type UserInput struct {
	Username string       `json:"username"`                // 用户名，1-32 位，可用字母、数字、下划线、点和横线
	Password string       `json:"password"`                // 密码；创建时必填且至少 6 位，更新时留空表示不修改
	Role     string       `json:"role" enums:"admin,user"` // 角色，admin 为管理员
	Disabled bool         `json:"disabled"`                // 是否禁用该账户
	Grants   []GrantInput `json:"grants"`                  // 授权桶列表，整表替换；更新时省略表示不修改
}

// GrantInput 把一个桶授权给用户。
//
// @Description 把一个桶授权给用户的请求体
type GrantInput struct {
	BucketID uint `json:"bucket_id" binding:"required"` // 桶 ID
	Writable bool `json:"writable"`                     // 是否可写；false 为只读
}

// BucketInput 创建 / 更新桶的请求体。
//
// @Description 创建或更新桶的请求体
type BucketInput struct {
	Name  string   `json:"name" binding:"required"`  // 桶名称，即用户虚拟根目录下显示的名称，最长 128 字符，不能含 / 或 \
	Paths []string `json:"paths" binding:"required"` // 服务器本地绝对路径，按优先级排列；多个目录互为备份，不能相互嵌套
}

// BucketDir 桶中一个目录的状态。
//
// @Description 桶中一个目录的状态
type BucketDir struct {
	Path      string `json:"path"`      // 服务器本地绝对路径
	Available bool   `json:"available"` // 目录当前是否存在且可访问
	Active    bool   `json:"active"`    // 是否为当前读写的主目录（第一个可用目录）
}

// BucketView 桶及其目录状态。
//
// @Description 桶及其目录状态
type BucketView struct {
	store.Bucket
	Users int         `json:"users"` // 被授权的用户数
	Dirs  []BucketDir `json:"dirs"`  // 各目录的可用状态
}

// BucketCreated 创建或更新桶的结果。
//
// @Description 创建或更新桶的结果
type BucketCreated struct {
	Bucket BucketView     `json:"bucket"` // 桶信息
	Sync   vfs.SyncResult `json:"sync"`   // 合并目录的结果
}

// BucketConflictResponse 目录合并冲突。
//
// @Description 目录合并冲突
type BucketConflictResponse struct {
	Error     string         `json:"error"`     // 错误信息
	Conflicts []vfs.Conflict `json:"conflicts"` // 冲突条目（最多 100 条）
}

// BrowseResponse 服务器本地目录浏览结果。
//
// @Description 服务器本地目录浏览结果
type BrowseResponse struct {
	Path   string   `json:"path"`   // 当前浏览的本地绝对路径
	Parent string   `json:"parent"` // 上一级本地路径；已在顶层时为空字符串
	Dirs   []string `json:"dirs"`   // 子目录的绝对路径
}

// CacheCleanRequest 清理缓存的请求体。
//
// @Description 清理缓存的请求体
type CacheCleanRequest struct {
	Kind string `json:"kind" enums:"thumb,transcode,all"` // 要清理的缓存桶：thumb 预览图，transcode 转码分片，all 全部
}

// CacheCleanResponse 清理结果。
//
// @Description 缓存清理结果
type CacheCleanResponse struct {
	RemovedFiles int   `json:"removed_files"` // 删除的文件数
	RemovedBytes int64 `json:"removed_bytes"` // 释放的字节数
}

// ReindexResponse 重建索引的受理结果。
//
// @Description 重建搜索索引的受理结果
type ReindexResponse struct {
	OK bool `json:"ok"` // 是否已开始后台重建
}

// ErrorResponse 错误响应。
//
// @Description 错误响应
type ErrorResponse struct {
	Error string `json:"error"` // 错误信息
}

// SystemInfo 系统与协议服务信息（用于挂载向导）。
//
// @Description 系统与协议服务信息，用于挂载向导
type SystemInfo struct {
	Version   string         `json:"version" extensions:"x-apifox-mock=1.2.3"`         // 服务器版本
	Hostname  string         `json:"hostname" extensions:"x-apifox-mock=mlist-server"` // 服务器主机名
	Addresses []string       `json:"addresses"`                                        // 服务器探测到的局域网 IP 地址
	HTTPPort  int            `json:"http_port" extensions:"x-apifox-mock=8080"`        // Web / API / WebDAV / S3 的监听端口
	FFmpeg    bool           `json:"ffmpeg" extensions:"x-apifox-mock=true"`           // 是否检测到 ffmpeg（决定转码与部分预览是否可用）
	Services  ServicesStatus `json:"services"`                                         // 各协议服务的状态
}

// CacheStats 缓存统计。
//
// @Description 缓存统计
type CacheStats struct {
	Dir         string              `json:"dir"`          // 缓存目录的绝对路径
	Thumb       cache.BucketStats   `json:"thumb"`        // 预览图缓存
	Transcode   cache.BucketStats   `json:"transcode"`    // 转码分片缓存
	Sessions    []media.SessionInfo `json:"sessions"`     // 当前已知的转码会话
	LastCleanup *time.Time          `json:"last_cleanup"` // 上次清理时间；null 表示尚未清理过
}

// IndexStatus 是搜索索引状态。
//
// @Description 搜索索引状态
type IndexStatus = search.Status

// CreateShareRequest 创建分享。
//
// @Description 创建分享的请求体
type CreateShareRequest struct {
	Path      string     `json:"path" binding:"required"` // 要分享的文件夹的虚拟路径
	ExpiresAt *time.Time `json:"expires_at"`              // 过期时间（RFC 3339）；null 或省略表示永不过期
}

// UpdateShareRequest 修改分享有效期。
//
// @Description 修改分享有效期的请求体
type UpdateShareRequest struct {
	ExpiresAt *time.Time `json:"expires_at"` // 新的过期时间（RFC 3339）；null 表示永不过期
}

// ShareView 分享记录。
//
// @Description 分享记录
type ShareView struct {
	store.Share
	Name   string `json:"name"`                                      // 被分享文件夹的名称
	Status string `json:"status" enums:"active,expired,unavailable"` // active 可访问；expired 已过期；unavailable 分享者已无权限或文件夹已不存在
}

// PublicShareInfo 公开的分享信息。
//
// @Description 公开的分享信息
type PublicShareInfo struct {
	Name      string     `json:"name"`       // 被分享文件夹的名称
	Owner     string     `json:"owner"`      // 分享者用户名
	ExpiresAt *time.Time `json:"expires_at"` // 过期时间；null 表示永不过期
}
