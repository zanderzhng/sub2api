//go:build !cgo

package service

import (
	"sync"

	"go.uber.org/zap"

	"github.com/klauspost/compress/zstd"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// piZstdWithoutCGO 是 CGO_ENABLED=0 目标（darwin / windows / arm64 发布产物）的压缩器：
// klauspost/compress 的 SpeedDefault 即 level 3。
//
// 与 libzstd 的差别（E1 实测）：帧头形状对齐（单段、帧内带内容长度、无 checksum，
// 与 pi 的 zlib.zstdCompressSync 相同），但码块分段与熵编码不同，
// 压缩结果长度会有百分之一量级的差异。因此**交付镜像必须用 cgo 实现**
// （linux/amd64 走 CGO_ENABLED=1），本实现只用于无法 cgo 的交叉编译目标，
// 保证这些产物仍能编译、压缩仍是合法的 zstd。
var piZstdWithoutCGO = sync.OnceValues(func() (*zstd.Encoder, error) {
	return zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderCRC(false),
		zstd.WithSingleSegment(true),
		zstd.WithEncoderConcurrency(1),
	)
})

// PiCompressZstd 返回 level 3 压缩后的 body；第二个返回值表示压缩是否可用。
func PiCompressZstd(body []byte) ([]byte, bool) {
	encoder, err := piZstdWithoutCGO()
	if err != nil {
		logger.L().Warn("pi zstd encoder unavailable", zap.Error(err))
		return nil, false
	}
	return encoder.EncodeAll(body, nil), true
}

// piZstdImplementation 供测试与日志声明当前生效的实现。
const piZstdImplementation = "klauspost-pure-go"
