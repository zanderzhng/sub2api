//go:build cgo

package service

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPiCompressZstdMatchesLibzstdLevel3 是 E1 判定的常驻回归：
// cgo 实现（libzstd）在 level 3 下的输出必须与 `zstd -3 --no-check`（等价于
// Node 的 zlib.zstdCompressSync(body, {level: 3})）逐字节一致。
// 帧内码流是实现指纹，一旦依赖升级或参数漂移就会在这里失败。
func TestPiCompressZstdMatchesLibzstdLevel3(t *testing.T) {
	require.Equal(t, "libzstd-cgo", piZstdImplementation)
	fixture, err := os.ReadFile("testdata/pi_zstd_level3_fixture.json")
	require.NoError(t, err)
	expected, err := os.ReadFile("testdata/pi_zstd_level3_fixture.expected.zst")
	require.NoError(t, err)
	compressed, ok := PiCompressZstd(fixture)
	require.True(t, ok)
	require.Equal(t, expected, compressed)
}
