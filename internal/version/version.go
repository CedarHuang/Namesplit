// Package version 持有构建时注入的版本信息。
//
// Version 与 Commit 由构建脚本通过 -ldflags "-X ..." 注入，源码中的默认值仅用于
// go run / go test 等未注入场景。
package version

import "fmt"

var (
	// Version 是构建版本号，例如 "1.0.0" 或 "0.0.0-dev+5d152f8"。
	Version = "0.0.0-dev"
	// Commit 是构建所基于的提交哈希，短格式。
	Commit = "unknown"
)

// String 返回便于展示与日志的版本串。
func String() string {
	return fmt.Sprintf("%s (%s)", Version, Commit)
}
