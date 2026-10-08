//go:build !cgo

package service

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPiCompressZstdPureGoFrameShape 声明纯 Go 回退实现的可见偏差：
// 帧头（FHD + 帧内内容长度）必须与 libzstd level 3 一致，码流不可比。
// 交付镜像走 cgo 实现（pi_compress.go），本实现只服务无法 cgo 的交叉编译目标。
func TestPiCompressZstdPureGoFrameShape(t *testing.T) {
	require.Equal(t, "klauspost-pure-go", piZstdImplementation)
	fixture, err := os.ReadFile("testdata/pi_zstd_level3_fixture.json")
	require.NoError(t, err)
	expected, err := os.ReadFile("testdata/pi_zstd_level3_fixture.expected.zst")
	require.NoError(t, err)
	compressed, ok := PiCompressZstd(fixture)
	require.True(t, ok)
	// magic + FHD + 2 字节内容长度：单段、无 checksum、帧内带长度，与 libzstd 相同。
	require.True(t, bytes.Equal(expected[:6], compressed[:6]),
		"frame header must match libzstd level 3: expected % x, got % x", expected[:6], compressed[:6])
	require.Equal(t, fixture, decodeZstdBody(t, compressed))
}
