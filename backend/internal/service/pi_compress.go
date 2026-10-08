//go:build cgo

package service

import (
	ddzstd "github.com/DataDog/zstd"
	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// pi 的 SSE 出站请求体压缩：zlib.zstdCompressSync(body, {level: 3})，即 libzstd level 3。
//
// 为什么必须是 libzstd 的字节级复刻（E1 实测结论）：纯 Go 的 klauspost/compress 在
// level 3 下能对齐帧头，但码块分段与熵编码不同（同一输入 363 字节 vs 368 字节），
// 帧内码流本身就是一个稳定的实现指纹——这正是伪装要消除的信号。
// DataDog/zstd 是 libzstd 的 cgo 绑定，level 3 输出与 `zstd -3 --no-check` 逐字节相同
// （见 pi_compress_libzstd_test.go）。
//
// 构建面：本文件只在 CGO_ENABLED=1 时参与编译；CGO_ENABLED=0 的交叉编译目标
// （darwin/windows/arm64 发布产物）回退到 pi_compress_pure.go 的纯 Go 实现。
// 交付的 linux/amd64 镜像必须走本文件：goreleaser 的该目标设 CGO_ENABLED=1
// （见 .goreleaser.simple.yaml 与 .goreleaser.yaml 的 linux/amd64 覆写）。
const piZstdCompressionLevel = 3

// PiCompressZstd 返回 libzstd level 3 压缩后的 body。
// 第二个返回值表示压缩是否可用：不可用时调用方发未压缩的 JSON（pi 在拿不到
// node:zlib 时的回退语义），这样单条请求不会因为压缩失败而失败。
func PiCompressZstd(body []byte) ([]byte, bool) {
	compressed, err := ddzstd.CompressLevel(nil, body, piZstdCompressionLevel)
	if err != nil {
		logger.L().Warn("pi zstd compress failed", zap.Error(err))
		return nil, false
	}
	return compressed, true
}

// piZstdImplementation 供测试与日志声明当前生效的实现（与真实 pi 的字节一致性据此判断）。
const piZstdImplementation = "libzstd-cgo"
