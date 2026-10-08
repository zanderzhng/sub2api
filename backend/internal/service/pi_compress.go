package service

import (
	ddzstd "github.com/DataDog/zstd"
	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// pi 的 SSE 出站请求体压缩：zlib.zstdCompressSync(body, {level: 3})，即 libzstd level 3。
//
// 为什么用 cgo 绑定而不是纯 Go 的 klauspost/compress（E1 实测结论）：
// klauspost 在 level 3 下产出的帧与 libzstd **不逐字节一致**（码块分段与熵编码不同，
// 同样的输入 363 字节 vs 368 字节），只有帧头能对齐；帧内码流是稳定的实现指纹，
// 这正是伪装要消除的信号。DataDog/zstd 是 libzstd 的直接绑定（C 源码进模块），
// level 3 的输出与 `zstd -3 --no-check` 逐字节相同，
// 见 pi_outbound_contract_test.go 的 TestPiCompressZstdMatchesLibzstdLevel3。
//
// 代价：二进制由 CGO_ENABLED=0 纯 Go 交叉编译改为 CGO_ENABLED=1（见 Dockerfile 的
// backend-builder）。运行镜像无需改动：C 代码静态链接进二进制，zstd-libs 本来就在。
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
