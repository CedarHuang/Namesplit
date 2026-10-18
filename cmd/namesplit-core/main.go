// Command namesplit-core 是 Namesplit 的 DNS 解析核心进程。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/CedarHuang/Namesplit/internal/app"
	"github.com/CedarHuang/Namesplit/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "打印版本并退出")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())

		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code := app.Run(ctx, app.Options{Token: os.Getenv("NAMESPLIT_TOKEN")})
	os.Exit(code)
}
