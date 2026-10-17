// Command namesplit-core 是 Namesplit 的 DNS 解析核心进程。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/CedarHuang/Namesplit/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "打印版本并退出")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	fmt.Fprintf(os.Stderr, "namesplit-core %s: DNS 解析尚未实现\n", version.String())
}
