//go:build !windows && !darwin

package fsident

import (
	"os"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// platformSnapshot 在项目不支持的平台（项目仅支持 darwin 与 windows）上恒不提供
// 文件实体标识：Identity 保持空串，跳过门对这类平台禁用（每次全读）。
// 保守取向：无法验证的文件系统语义一律不给实体证据。
func platformSnapshot(path string) model.FileSnapshot {
	fi, err := os.Stat(path)
	if err != nil {
		return model.FileSnapshot{}
	}
	return model.FileSnapshot{MtimeNS: fi.ModTime().UnixNano(), Size: fi.Size()}
}
