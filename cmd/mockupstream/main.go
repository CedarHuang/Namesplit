// Command mockupstream 是开发与验收用的本地 DNS 上游。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CedarHuang/Namesplit/internal/mockdns"
)

func main() {
	answer := flag.String("a", "192.0.2.1", "A 查询应答地址")
	ttl := flag.Uint("ttl", 60, "应答 TTL")
	delay := flag.Duration("delay", 0, "应答前延迟")
	encrypted := flag.Bool("encrypted", false, "同时启动 DoH/DoT/DoQ/DoH3")
	flag.Parse()

	ip := net.ParseIP(*answer)
	if ip == nil {
		fmt.Fprintf(os.Stderr, "非法的 -a 地址: %s\n", *answer)

		os.Exit(2)
	}

	plain, err := mockdns.StartPlain(mockdns.PlainConfig{
		A:     ip,
		TTL:   uint32(*ttl),
		Delay: *delay,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动明文上游失败: %v\n", err)

		os.Exit(1)
	}
	defer func() { _ = plain.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	payload := map[string]string{"udp": plain.URL()}

	if *encrypted {
		mock, err := mockdns.StartEncrypted(ctx, plain.URL())
		if err != nil {
			fmt.Fprintf(os.Stderr, "启动加密上游失败: %v\n", err)

			os.Exit(1)
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = mock.Close(shutdownCtx)
		}()

		payload["doh"] = mock.DoHURL()
		payload["dot"] = mock.DoTURL()
		payload["doq"] = mock.DoQURL()
		if mock.DoH3URL() != "" {
			payload["doh3"] = mock.DoH3URL()
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "序列化地址失败: %v\n", err)

		os.Exit(1)
	}
	fmt.Println(string(data))

	<-ctx.Done()
}
